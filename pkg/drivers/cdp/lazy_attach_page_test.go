package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/protocol/runtime"
	"github.com/mafredri/cdp/rpcc"
	"github.com/rs/zerolog"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/eval"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/templates"
	rtEvents "github.com/MontFerret/ferret/pkg/runtime/events"
)

// fakeNavigation is a navigation event stream that sends one event after a delay, or never.
type fakeNavigation struct {
	steps    *[]string
	delay    time.Duration
	never    bool
	err      error
	released int
}

func (n *fakeNavigation) Read(ctx context.Context) <-chan rtEvents.Message {
	ch := make(chan rtEvents.Message, 1)

	go func() {
		if n.never {
			<-ctx.Done()
			close(ch)

			return
		}

		select {
		case <-time.After(n.delay):
			if n.err != nil {
				ch <- rtEvents.WithErr(n.err)

				return
			}

			*n.steps = append(*n.steps, "first navigation event")
			ch <- rtEvents.WithValue(nil)
		case <-ctx.Done():
			close(ch)
		}
	}()

	return ch
}

func (n *fakeNavigation) Release() error {
	n.released++

	return nil
}

// lazySteps records the order of the steps of a lazyAttach document.
type lazySteps struct {
	steps     []string
	nav       *fakeNavigation
	committed bool
	parsedErr error
	evalErr   error
	rootErr   error
}

func newLazySteps(committed bool) *lazySteps {
	s := &lazySteps{committed: committed}
	s.nav = &fakeNavigation{steps: &s.steps}

	return s
}

func (s *lazySteps) build() lazyDocumentSteps {
	return lazyDocumentSteps{
		navigation: func(_ context.Context) (navigationEvents, error) {
			s.steps = append(s.steps, "subscribe")

			return s.nav, nil
		},
		committed: func(_ context.Context) (bool, error) {
			s.steps = append(s.steps, "committed?")

			return s.committed, nil
		},
		evaluate: func(_ context.Context) error {
			s.steps = append(s.steps, "evaluateArgs")

			return s.evalErr
		},
		waitParsed: func(_ context.Context) error {
			s.steps = append(s.steps, "wait parsed")

			return s.parsedErr
		},
		loadRoot: func(_ context.Context) error {
			s.steps = append(s.steps, "load root")

			return s.rootErr
		},
	}
}

func checkSteps(t *testing.T, got []string, want ...string) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps %q, want %q", got, want)
	}
}

func TestLazyDocumentCommitted(t *testing.T) {
	s := newLazySteps(true)
	report := newLazyAttachReport(drivers.Params{})

	if err := s.build().run(context.Background(), false, report); err != nil {
		t.Fatal(err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?", "evaluateArgs", "wait parsed", "load root")

	if s.nav.released != 1 || report.committed != "yes" || report.ready != "ready" {
		t.Errorf("released %d, report %+v", s.nav.released, report)
	}
}

// TestLazyDocumentNotCommitted: the user script runs after the first navigation event,
// otherwise it would run in the initial blank document and be lost.
func TestLazyDocumentNotCommitted(t *testing.T) {
	s := newLazySteps(false)
	s.nav.delay = 20 * time.Millisecond
	report := newLazyAttachReport(drivers.Params{})

	if err := s.build().run(context.Background(), false, report); err != nil {
		t.Fatal(err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?", "first navigation event", "evaluateArgs", "wait parsed", "load root")

	if s.nav.released != 1 || report.committed != "no" {
		t.Errorf("released %d, report %+v", s.nav.released, report)
	}
}

func TestLazyDocumentSkipParsed(t *testing.T) {
	s := newLazySteps(true)
	report := newLazyAttachReport(drivers.Params{})

	if err := s.build().run(context.Background(), true, report); err != nil {
		t.Fatal(err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?", "evaluateArgs", "load root")

	if report.ready != "skipped" {
		t.Errorf("report %+v, want the wait for parsing skipped", report)
	}
}

// TestLazyDocumentSkipParsedNotCommitted: waitReadyOnURL false does not skip the first
// navigation event, otherwise the initial blank document would be loaded.
func TestLazyDocumentSkipParsedNotCommitted(t *testing.T) {
	s := newLazySteps(false)

	if err := s.build().run(context.Background(), true, newLazyAttachReport(drivers.Params{})); err != nil {
		t.Fatal(err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?", "first navigation event", "evaluateArgs", "load root")
}

func TestLazyDocumentDeadlineInFirstNavigation(t *testing.T) {
	s := newLazySteps(false)
	s.nav.never = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	report := newLazyAttachReport(drivers.Params{})

	err := s.build().run(ctx, false, report)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context error", err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?")

	if s.nav.released != 1 || report.stage != stageFirstNavigation || report.ready != notReached {
		t.Errorf("released %d, report %+v", s.nav.released, report)
	}
}

func TestLazyDocumentParsedError(t *testing.T) {
	s := newLazySteps(true)
	s.parsedErr = errors.New("Execution context was destroyed")
	report := newLazyAttachReport(drivers.Params{})

	err := s.build().run(context.Background(), false, report)

	if err == nil {
		t.Fatal("an error of the wait for parsing must end the call")
	}

	checkSteps(t, s.steps, "subscribe", "committed?", "evaluateArgs", "wait parsed")

	if report.stage != stageWaitParsed || report.ready != "error" {
		t.Errorf("report %+v", report)
	}
}

func TestSkipParsedFor(t *testing.T) {
	cases := []struct {
		reason string
		wait   bool
		skip   bool
	}{
		{verdictURL, true, false},
		{verdictURL, false, true},
		{verdictTitle, false, false},
		{verdictTimeout, false, false},
		{verdictTitle, true, false},
	}

	for _, c := range cases {
		opts := &drivers.LazyAttach{WaitReadyOnURL: c.wait}

		if got := skipParsedFor(lazyVerdict{Reason: c.reason}, opts); got != c.skip {
			t.Errorf("verdict %s, waitReadyOnURL %v: skip %v, want %v", c.reason, c.wait, got, c.skip)
		}
	}
}

// staleContextText is the text Chrome gives for a call into a destroyed execution context.
const staleContextText = "Execution context was destroyed."

// fakeRuntime answers Runtime.callFunctionOn from a script: "parsed", "loading", "stale" or an
// error text, repeating the last answer.
type fakeRuntime struct {
	cdp.Runtime
	mu      sync.Mutex
	answers []string
	calls   int
}

func (r *fakeRuntime) CallFunctionOn(_ context.Context, args *runtime.CallFunctionOnArgs) (*runtime.CallFunctionOnReply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The script answers only the end-of-parsing condition, so a wait for another condition,
	// such as complete, fails the test.
	if !strings.Contains(args.FunctionDeclaration, "document.readyState !== 'loading'") {
		return nil, errors.New("unexpected function: " + args.FunctionDeclaration)
	}

	a := r.answers[len(r.answers)-1]

	if r.calls < len(r.answers) {
		a = r.answers[r.calls]
	}

	r.calls++

	switch a {
	case "parsed":
		return &runtime.CallFunctionOnReply{Result: runtime.RemoteObject{Type: "boolean", Value: json.RawMessage("true")}}, nil
	case "loading":
		return &runtime.CallFunctionOnReply{Result: runtime.RemoteObject{Type: "object"}}, nil
	case "stale":
		return nil, errors.New(staleContextText)
	case "exception":
		desc := "TypeError: document is not defined"

		return &runtime.CallFunctionOnReply{
			Result:           runtime.RemoteObject{Type: "undefined"},
			ExceptionDetails: &runtime.ExceptionDetails{Text: "Uncaught", Exception: &runtime.RemoteObject{Type: "object", Description: &desc}},
		}, nil
	default:
		return nil, errors.New(a)
	}
}

type fakeWorlds struct {
	cdp.Page
	err   error
	calls int
}

func (p *fakeWorlds) CreateIsolatedWorld(_ context.Context, _ *page.CreateIsolatedWorldArgs) (*page.CreateIsolatedWorldReply, error) {
	p.calls++

	if p.err != nil {
		return nil, p.err
	}

	return &page.CreateIsolatedWorldReply{ExecutionContextID: runtime.ExecutionContextID(100 + p.calls)}, nil
}

func parsedRuntime(answers ...string) (*eval.Runtime, *fakeRuntime, *fakeWorlds) {
	rt := &fakeRuntime{answers: answers}
	worlds := &fakeWorlds{}
	client := &cdp.Client{Runtime: rt, Page: worlds}

	return eval.New(zerolog.Nop(), client, "F1", 1), rt, worlds
}

func TestWaitParsedReturnsAtOnce(t *testing.T) {
	ec, rt, _ := parsedRuntime("parsed")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err != nil || rt.calls != 1 {
		t.Errorf("got %v after %d calls, want one call", err, rt.calls)
	}
}

func TestWaitParsedWaitsWhileLoading(t *testing.T) {
	ec, rt, _ := parsedRuntime("loading", "loading", "parsed")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err != nil || rt.calls != 3 {
		t.Errorf("got %v after %d calls, want three calls", err, rt.calls)
	}
}

// TestWaitParsedDocumentChangedBetweenPolls: a stale context recreates the isolated world and the
// call is repeated in the new document.
func TestWaitParsedDocumentChangedBetweenPolls(t *testing.T) {
	ec, rt, worlds := parsedRuntime("loading", "stale", "parsed")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err != nil || worlds.calls != 1 || rt.calls != 3 {
		t.Errorf("got %v after %d calls and %d new worlds", err, rt.calls, worlds.calls)
	}
}

// TestWaitParsedCallAndRetryBothStale: the retry is one per call and the wait ends on the first
// error, so a change of document hit by the call and its retry ends the wait with an error.
func TestWaitParsedCallAndRetryBothStale(t *testing.T) {
	ec, _, _ := parsedRuntime("stale", "stale", "parsed")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err == nil {
		t.Error("want an error")
	}
}

func TestWaitParsedWorldRecreationFails(t *testing.T) {
	ec, _, worlds := parsedRuntime("stale", "parsed")
	worlds.err = errors.New("Cannot find frame with given id")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err == nil {
		t.Error("want an error")
	}
}

func TestWaitParsedDeadline(t *testing.T) {
	ec, _, _ := parsedRuntime("loading")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := waitParsed(ctx, ec, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the context error", err)
	}
}

// TestWaitParsedPageException: an exception in the page comes as a reply with ExceptionDetails,
// not as a transport error, and it ends the wait too.
func TestWaitParsedPageException(t *testing.T) {
	ec, _, _ := parsedRuntime("exception")

	if err := waitParsed(context.Background(), ec, 10*time.Millisecond); err == nil {
		t.Error("want an error")
	}
}

// TestDOMParsedTemplate guards the condition itself: the end of HTML parsing is any readyState
// but loading, and complete is not waited for. The fake runtime answers only this condition, so
// waitParsed is checked to use it.
func TestDOMParsedTemplate(t *testing.T) {
	src := templates.DOMParsed().String()

	if !strings.Contains(src, "document.readyState !== 'loading'") || strings.Contains(src, "complete") {
		t.Errorf("unexpected template %q", src)
	}
}

func TestPageLoadStepFor(t *testing.T) {
	urlVerdict := newLazyAttachReport(drivers.Params{})
	urlVerdict.setVerdict(lazyVerdict{Reason: verdictURL})
	titleVerdict := newLazyAttachReport(drivers.Params{})
	titleVerdict.setVerdict(lazyVerdict{Reason: verdictTitle})
	skipOnURL := &drivers.LazyAttach{WaitReadyOnURL: false}
	waitOnURL := &drivers.LazyAttach{WaitReadyOnURL: true}

	cases := []struct {
		name   string
		params drivers.Params
		report *lazyAttachReport
		step   pageLoadStep
		skip   bool
	}{
		{"blank", drivers.Params{URL: BlankPageURL, DirectNavigation: true, LazyAttach: skipOnURL}, urlVerdict, stepLoadBlank, false},
		{"empty url", drivers.Params{}, nil, stepLoadBlank, false},
		{"navigate", drivers.Params{URL: waitTestURL}, nil, stepNavigate, false},
		{"direct without lazyAttach", drivers.Params{URL: waitTestURL, DirectNavigation: true}, nil, stepDirect, false},
		{"lazy, url, waitReadyOnURL false", drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL}, urlVerdict, stepLazy, true},
		{"lazy, url, waitReadyOnURL true", drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: waitOnURL}, urlVerdict, stepLazy, false},
		{"lazy, title, waitReadyOnURL false", drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL}, titleVerdict, stepLazy, false},
		{"lazy without report", drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL}, nil, stepLazy, false},
	}

	for _, c := range cases {
		step, skip := pageLoadStepFor(c.params, c.report)

		if step != c.step || skip != c.skip {
			t.Errorf("%s: got step %v, skip %v, want %v, %v", c.name, step, skip, c.step, c.skip)
		}
	}
}

func TestLazyDocumentStepErrorsMarkStage(t *testing.T) {
	ev := newLazySteps(true)
	ev.evalErr = errors.New("Uncaught")
	report := newLazyAttachReport(drivers.Params{})

	if err := ev.build().run(context.Background(), false, report); err == nil || report.stage != stageEvaluate || report.ready != notReached {
		t.Errorf("evaluateArgs: got %v, report %+v", err, report)
	}

	root := newLazySteps(true)
	root.rootErr = errors.New("no root")
	report = newLazyAttachReport(drivers.Params{})

	if err := root.build().run(context.Background(), false, report); err == nil || report.stage != stageLoadRoot || report.ready != "ready" {
		t.Errorf("load root: got %v, report %+v", err, report)
	}
}

// TestLazyDocumentFirstNavigationStreamError: an error message of the navigation stream, such as a
// closed connection, ends the call at the first navigation stage instead of passing as an event.
func TestLazyDocumentFirstNavigationStreamError(t *testing.T) {
	s := newLazySteps(false)
	s.nav.err = rpcc.ErrConnClosing
	report := newLazyAttachReport(drivers.Params{})

	err := s.build().run(context.Background(), false, report)

	if !errors.Is(err, rpcc.ErrConnClosing) {
		t.Fatalf("got %v, want the stream error", err)
	}

	checkSteps(t, s.steps, "subscribe", "committed?")

	if report.stage != stageFirstNavigation || report.ready != notReached || s.nav.released != 1 {
		t.Errorf("released %d, report %+v", s.nav.released, report)
	}
}
