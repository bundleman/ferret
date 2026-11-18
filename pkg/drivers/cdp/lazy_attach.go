package cdp

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mafredri/cdp/protocol/target"
	"github.com/rs/zerolog"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/eval"
	cdpEvents "github.com/MontFerret/ferret/pkg/drivers/cdp/events"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/templates"
	rtEvents "github.com/MontFerret/ferret/pkg/runtime/events"
)

// How the wait for a lazyAttach verdict ended.
const (
	verdictURL       = "url"
	verdictTitle     = "title"
	verdictTimeout   = "timeout"
	verdictPollError = "poll_error"
	verdictCanceled  = "canceled"
	// verdictBeforePoll is written for a call that failed before the first poll.
	verdictBeforePoll = "before_poll"
)

// lazyVerdict is the result of waiting for a lazyAttach verdict.
type lazyVerdict struct {
	Reason string
	// Pattern is the url or title pattern that matched.
	Pattern string
	// URL and Title are the tab address and title of the last poll with a non-empty address.
	URL   string
	Title string
	// AttachedSeen reports whether any poll saw the tab attached.
	AttachedSeen bool
	Wait         time.Duration
}

// tabPoll reads the address, title and attached flag of the tab without attaching to it.
type tabPoll func(ctx context.Context) (url, title string, attached bool, err error)

// awaitLazyVerdict polls the tab until its address matches a url pattern, its real title
// matches a title pattern or opts.Timeout passes. The first poll is made right away, the next
// ones opts.Interval after the previous poll ends. The end of opts.Timeout, also during a poll
// request, is not an error. A poll error ends the wait with that error, and the end of ctx
// with the context error.
func awaitLazyVerdict(ctx context.Context, opts *drivers.LazyAttach, requested string, poll tabPoll) (lazyVerdict, error) {
	start := time.Now()
	res := lazyVerdict{}

	urlPatterns, err := compileLazyPatterns(opts.URL)

	if err != nil {
		return res, err
	}

	titlePatterns, err := compileLazyPatterns(opts.Title)

	if err != nil {
		return res, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	addrs := newTabAddresses(requested)

	// ended tells the end of ctx, which is an error, from the end of opts.Timeout, which is not.
	ended := func() (lazyVerdict, error) {
		res.Wait = time.Since(start)

		if err := ctx.Err(); err != nil {
			res.Reason = verdictCanceled

			return res, err
		}

		res.Reason = verdictTimeout

		return res, nil
	}

	for {
		tabURL, title, attached, err := poll(waitCtx)

		if err != nil {
			if waitCtx.Err() != nil {
				return ended()
			}

			res.Reason = verdictPollError
			res.Wait = time.Since(start)

			return res, err
		}

		res.AttachedSeen = res.AttachedSeen || attached

		// Between two documents the address is empty for a moment: nothing is checked then.
		if tabURL != "" {
			res.URL, res.Title = tabURL, title
			addrs.add(tabURL)

			if p := firstMatch(opts.URL, urlPatterns, tabURL); p != "" {
				res.Reason, res.Pattern, res.Wait = verdictURL, p, time.Since(start)

				return res, nil
			}

			if !addrs.isPlaceholder(title) {
				if p := firstMatch(opts.Title, titlePatterns, title); p != "" {
					res.Reason, res.Pattern, res.Wait = verdictTitle, p, time.Since(start)

					return res, nil
				}
			}
		}

		timer := time.NewTimer(opts.Interval)

		select {
		case <-waitCtx.Done():
			timer.Stop()

			return ended()
		case <-timer.C:
		}
	}
}

func compileLazyPatterns(patterns []string) ([]*regexp.Regexp, error) {
	res := make([]*regexp.Regexp, 0, len(patterns))

	for _, p := range patterns {
		re, err := drivers.CompileLazyPattern(p)

		if err != nil {
			return nil, err
		}

		res = append(res, re)
	}

	return res, nil
}

func firstMatch(patterns []string, compiled []*regexp.Regexp, s string) string {
	for i, re := range compiled {
		if re.MatchString(s) {
			return patterns[i]
		}
	}

	return ""
}

// tabAddresses remembers every address a tab has shown since it was created, to tell its
// real title from Chrome's placeholder. While a document has no <title> of its own, Chrome
// shows the tab address instead: without the scheme, the site root as the bare host, with
// some %XX sequences decoded, and for a while still the address of the previous document.
type tabAddresses struct {
	forms map[string]struct{}
}

func newTabAddresses(requested string) *tabAddresses {
	a := &tabAddresses{forms: make(map[string]struct{})}
	a.add(requested)

	return a
}

// add remembers the address in three forms: whole, without the scheme and host only.
// An empty address, seen between two documents, is skipped.
func (a *tabAddresses) add(address string) {
	if address == "" {
		return
	}

	a.forms[decodePercent(address)] = struct{}{}

	rest := address

	if i := strings.Index(address, "://"); i >= 0 {
		rest = address[i+3:]
		a.forms[decodePercent(rest)] = struct{}{}
	}

	// The host is cut by hand: url.Parse rejects an address with an unpaired '%' in its path.
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}

	if rest != "" {
		a.forms[decodePercent(rest)] = struct{}{}
	}
}

// isPlaceholder reports whether the title is empty or one of the forms of an address the tab
// has shown, compared after decoding %XX on both sides.
func (a *tabAddresses) isPlaceholder(title string) bool {
	if title == "" {
		return true
	}

	_, ok := a.forms[decodePercent(title)]

	return ok
}

// decodePercent decodes valid %XX sequences in one pass. Anything else, including '%' without
// two hex digits and '+', stays as is, and the result is not decoded again.
func decodePercent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}

	b := make([]byte, 0, len(s))

	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2

			continue
		}

		b = append(b, s[i])
	}

	return string(b)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// Stages of a lazyAttach call, for the record in the ferret log.
const (
	stagePrepare         = "prepare"
	stagePoll            = "poll"
	stageAttach          = "attach"
	stageSessionSetup    = "session_setup"
	stageFirstNavigation = "first_navigation"
	stageEvaluate        = "evaluate_args"
	stageWaitParsed      = "wait_parsed"
	stageLoadRoot        = "load_root_document"
	// notReached marks a step the call did not come to.
	notReached = "not_reached"
)

// lazyAttachReport collects what happened in one DOCUMENT() call with lazyAttach. A nil report
// means the call has no lazyAttach, and every method is a no-op then.
type lazyAttachReport struct {
	start     time.Time
	url       string
	targetID  target.ID
	incognito bool
	verdict   lazyVerdict
	committed string
	ready     string
	readyWait time.Duration
	stage     string
	err       error
}

func newLazyAttachReport(params drivers.Params) *lazyAttachReport {
	return &lazyAttachReport{
		start:     time.Now(),
		url:       params.URL,
		verdict:   lazyVerdict{Reason: verdictBeforePoll},
		committed: notReached,
		ready:     notReached,
	}
}

func (r *lazyAttachReport) setIncognito(incognito bool) {
	if r != nil {
		r.incognito = incognito
	}
}

func (r *lazyAttachReport) setTarget(id target.ID) {
	if r != nil {
		r.targetID = id
	}
}

func (r *lazyAttachReport) setVerdict(v lazyVerdict) {
	if r != nil {
		r.verdict = v
	}
}

// fail records the first error of the call and the stage it happened on.
func (r *lazyAttachReport) fail(stage string, err error) {
	if r != nil && r.err == nil {
		r.stage, r.err = stage, err
	}
}

func (r *lazyAttachReport) setCommitted(committed bool) {
	if r == nil {
		return
	}

	r.committed = "no"

	if committed {
		r.committed = "yes"
	}
}

func (r *lazyAttachReport) setParsed(state string, wait time.Duration) {
	if r != nil {
		r.ready, r.readyWait = state, wait
	}
}

// log writes one info record about the call. err is the error the call ended with; one the steps
// did not mark is written with the stage unknown.
func (r *lazyAttachReport) log(logger zerolog.Logger, err error) {
	if r == nil {
		return
	}

	result, stage, text := "ok", "", ""

	if err != nil {
		r.fail("unknown", err)
		result, stage, text = "error", r.stage, r.err.Error()
	}

	logger.Info().
		Str("target_id", string(r.targetID)).
		Bool("incognito", r.incognito).
		Str("url", r.url).
		Str("verdict", r.verdict.Reason).
		Str("pattern", r.verdict.Pattern).
		Str("verdict_url", r.verdict.URL).
		Str("verdict_title", r.verdict.Title).
		Bool("attached_seen", r.verdict.AttachedSeen).
		Int64("verdict_wait_ms", r.verdict.Wait.Milliseconds()).
		Str("committed", r.committed).
		Str("parsed", r.ready).
		Int64("parsed_wait_ms", r.readyWait.Milliseconds()).
		Str("result", result).
		Str("stage", stage).
		Str("error", text).
		Int64("total_ms", time.Since(r.start).Milliseconds()).
		Msg("lazyAttach document")
}

// pageLoadStep is how a new page is loaded after its tab is attached.
type pageLoadStep int

const (
	// stepLoadBlank loads the document of a tab opened on about:blank.
	stepLoadBlank pageLoadStep = iota
	// stepNavigate sends Page.navigate, as without directNavigation.
	stepNavigate
	// stepDirect waits for the commit of a tab opened on the target URL, as directNavigation: true.
	stepDirect
	// stepLazy finishes a tab attached after its lazyAttach verdict.
	stepLazy
)

// pageLoadStepFor chooses how the page is loaded and, for lazyAttach, whether the wait for the end
// of HTML parsing is skipped. Without a report the verdict is unknown and nothing is skipped.
func pageLoadStepFor(params drivers.Params, report *lazyAttachReport) (pageLoadStep, bool) {
	switch {
	case params.URL == BlankPageURL || params.URL == "":
		return stepLoadBlank, false
	case !params.DirectNavigation:
		return stepNavigate, false
	case params.LazyAttach == nil:
		return stepDirect, false
	}

	var verdict lazyVerdict

	if report != nil {
		verdict = report.verdict
	}

	return stepLazy, skipParsedFor(verdict, params.LazyAttach)
}

// skipParsedFor reports whether the wait for the end of HTML parsing is skipped: only after a
// verdict by url and only with waitReadyOnURL false.
func skipParsedFor(v lazyVerdict, opts *drivers.LazyAttach) bool {
	return v.Reason == verdictURL && !opts.WaitReadyOnURL
}

type (
	// navigationEvents is a navigation event stream that can be stopped without waiting for
	// the document.
	navigationEvents interface {
		Read(ctx context.Context) <-chan rtEvents.Message
		Release() error
	}

	// lazyDocumentSteps are the steps of opening a document of a tab attached after its verdict.
	lazyDocumentSteps struct {
		navigation func(ctx context.Context) (navigationEvents, error)
		committed  func(ctx context.Context) (bool, error)
		evaluate   func(ctx context.Context) error
		waitParsed func(ctx context.Context) error
		loadRoot   func(ctx context.Context) error
	}
)

// run waits for the first navigation event if the document is not committed yet, runs the user
// script, waits for the end of HTML parsing unless skipParsed and loads the root document. It
// never waits for complete, and any error ends the call, so the caller closes the tab instead of
// returning a page in the middle of parsing.
func (s lazyDocumentSteps) run(ctx context.Context, skipParsed bool, report *lazyAttachReport) error {
	// The stream is opened before the frame is inspected, otherwise a commit landing between the
	// check and the subscription would be waited for forever.
	nav, err := s.navigation(ctx)

	if err != nil {
		report.fail(stageFirstNavigation, err)

		return err
	}

	// Released, not closed: Close would wait for the document to complete.
	defer nav.Release()

	committed, err := s.committed(ctx)

	if err != nil {
		report.fail(stageFirstNavigation, err)

		return err
	}

	report.setCommitted(committed)

	if !committed {
		// Without it the initial blank document would be loaded, so it is waited for even when
		// the wait for parsing is skipped.
		readCtx, cancel := context.WithCancel(ctx)

		var streamErr error

		for msg := range nav.Read(readCtx) {
			// A closed session or connection comes as an error message, not as an event.
			streamErr = msg.Err()

			break
		}

		cancel()

		if err := ctx.Err(); err != nil {
			streamErr = err
		}

		if streamErr != nil {
			report.fail(stageFirstNavigation, streamErr)

			return streamErr
		}
	}

	if err := s.evaluate(ctx); err != nil {
		report.fail(stageEvaluate, err)

		return err
	}

	if skipParsed {
		report.setParsed("skipped", 0)
	} else {
		start := time.Now()

		if err := s.waitParsed(ctx); err != nil {
			report.setParsed("error", time.Since(start))
			report.fail(stageWaitParsed, err)

			return err
		}

		report.setParsed("ready", time.Since(start))
	}

	if err := s.loadRoot(ctx); err != nil {
		report.fail(stageLoadRoot, err)

		return err
	}

	return nil
}

// waitParsed waits until the current document of the frame has parsed its HTML. A stale context
// recreates the isolated world and repeats the call once; a stale error of the retry, like any other
// error, ends the wait.
func waitParsed(ctx context.Context, ec *eval.Runtime, polling time.Duration) error {
	_, err := cdpEvents.NewEvalWaitTask(ec, templates.DOMParsed(), polling).Run(ctx)

	return err
}
