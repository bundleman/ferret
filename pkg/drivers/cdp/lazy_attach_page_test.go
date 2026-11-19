package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/protocol/runtime"
	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	. "github.com/smartystreets/goconvey/convey"

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

func Test_LazyDocumentSteps_Run(t *testing.T) {
	type fields struct {
		committed bool
		navDelay  time.Duration
		navNever  bool
		navErr    error
		parsedErr error
		evalErr   error
		rootErr   error
	}
	type args struct {
		skipParsed bool
		deadline   time.Duration
	}
	errParsed := errors.New("Execution context was destroyed")
	errEval := errors.New("Uncaught")
	errRoot := errors.New("no root")
	tests := []struct {
		name      string
		fields    fields
		args      args
		wantErr   error
		wantSteps []string
		assert    func(s *lazySteps, report *lazyAttachReport)
	}{
		{
			name:      "Should run every step of a committed document",
			fields:    fields{committed: true},
			wantSteps: []string{"subscribe", "committed?", "evaluateArgs", "wait parsed", "load root"},
			assert: func(s *lazySteps, report *lazyAttachReport) {
				So(s.nav.released, ShouldEqual, 1)
				So(report.committed, ShouldEqual, "yes")
				So(report.ready, ShouldEqual, "ready")
			},
		},
		{
			// Otherwise the user script would run in the initial blank document and be lost.
			name:      "Should run the user script after the first navigation event of a document not committed yet",
			fields:    fields{committed: false, navDelay: 20 * time.Millisecond},
			wantSteps: []string{"subscribe", "committed?", "first navigation event", "evaluateArgs", "wait parsed", "load root"},
			assert: func(s *lazySteps, report *lazyAttachReport) {
				So(s.nav.released, ShouldEqual, 1)
				So(report.committed, ShouldEqual, "no")
			},
		},
		{
			name:      "Should skip the wait for parsing",
			fields:    fields{committed: true},
			args:      args{skipParsed: true},
			wantSteps: []string{"subscribe", "committed?", "evaluateArgs", "load root"},
			assert: func(_ *lazySteps, report *lazyAttachReport) {
				So(report.ready, ShouldEqual, "skipped")
			},
		},
		{
			// Otherwise the initial blank document would be loaded.
			name:      "Should wait for the first navigation event also when the wait for parsing is skipped",
			fields:    fields{committed: false},
			args:      args{skipParsed: true},
			wantSteps: []string{"subscribe", "committed?", "first navigation event", "evaluateArgs", "load root"},
			assert:    func(*lazySteps, *lazyAttachReport) {},
		},
		{
			name:      "Should end at the first navigation stage when the call ends",
			fields:    fields{committed: false, navNever: true},
			args:      args{deadline: 30 * time.Millisecond},
			wantErr:   context.DeadlineExceeded,
			wantSteps: []string{"subscribe", "committed?"},
			assert: func(s *lazySteps, report *lazyAttachReport) {
				So(s.nav.released, ShouldEqual, 1)
				So(report.stage, ShouldEqual, stageFirstNavigation)
				So(report.ready, ShouldEqual, notReached)
			},
		},
		{
			name:      "Should end the call on an error of the wait for parsing",
			fields:    fields{committed: true, parsedErr: errParsed},
			wantErr:   errParsed,
			wantSteps: []string{"subscribe", "committed?", "evaluateArgs", "wait parsed"},
			assert: func(_ *lazySteps, report *lazyAttachReport) {
				So(report.stage, ShouldEqual, stageWaitParsed)
				So(report.ready, ShouldEqual, "error")
			},
		},
		{
			name:    "Should mark the evaluate stage on an error of the user script",
			fields:  fields{committed: true, evalErr: errEval},
			wantErr: errEval,
			assert: func(_ *lazySteps, report *lazyAttachReport) {
				So(report.stage, ShouldEqual, stageEvaluate)
				So(report.ready, ShouldEqual, notReached)
			},
		},
		{
			name:    "Should mark the load root stage on an error of loading the root document",
			fields:  fields{committed: true, rootErr: errRoot},
			wantErr: errRoot,
			assert: func(_ *lazySteps, report *lazyAttachReport) {
				So(report.stage, ShouldEqual, stageLoadRoot)
				So(report.ready, ShouldEqual, "ready")
			},
		},
		{
			// A closed connection comes as an error message of the stream, not as an event.
			name:      "Should end at the first navigation stage on an error of the navigation stream",
			fields:    fields{committed: false, navErr: rpcc.ErrConnClosing},
			wantErr:   rpcc.ErrConnClosing,
			wantSteps: []string{"subscribe", "committed?"},
			assert: func(s *lazySteps, report *lazyAttachReport) {
				So(report.stage, ShouldEqual, stageFirstNavigation)
				So(report.ready, ShouldEqual, notReached)
				So(s.nav.released, ShouldEqual, 1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				s := newLazySteps(tt.fields.committed)
				s.nav.delay, s.nav.never, s.nav.err = tt.fields.navDelay, tt.fields.navNever, tt.fields.navErr
				s.parsedErr, s.evalErr, s.rootErr = tt.fields.parsedErr, tt.fields.evalErr, tt.fields.rootErr
				report := newLazyAttachReport(drivers.Params{})

				// A broken wait must fail the case, not hang the test.
				deadline := tt.args.deadline
				if deadline == 0 {
					deadline = 5 * time.Second
				}

				ctx, cancel := context.WithTimeout(context.Background(), deadline)
				t.Cleanup(cancel)

				err := s.build().run(ctx, tt.args.skipParsed, report)

				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
				} else {
					So(err, ShouldBeNil)
				}

				if tt.wantSteps != nil {
					So(s.steps, ShouldResemble, tt.wantSteps)
				}

				tt.assert(s, report)
			})
		})
	}
}

func Test_SkipParsedFor(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		wait   bool
		want   bool
	}{
		{name: "Should wait after a verdict by url with waitReadyOnURL", reason: verdictURL, wait: true, want: false},
		{name: "Should skip after a verdict by url without waitReadyOnURL", reason: verdictURL, wait: false, want: true},
		{name: "Should wait after a verdict by title without waitReadyOnURL", reason: verdictTitle, wait: false, want: false},
		{name: "Should wait after a timeout without waitReadyOnURL", reason: verdictTimeout, wait: false, want: false},
		{name: "Should wait after a verdict by title with waitReadyOnURL", reason: verdictTitle, wait: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				So(skipParsedFor(lazyVerdict{Reason: tt.reason}, &drivers.LazyAttach{WaitReadyOnURL: tt.wait}), ShouldEqual, tt.want)
			})
		})
	}
}

// staleContextText is the text Chrome gives for a call into a destroyed execution context, the one
// eval.IsStaleContextErr matches, so it keeps Chrome's capital letter and period.
const staleContextText = "Execution context was destroyed."

var errStaleContext = errors.New(staleContextText)

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
		return nil, errStaleContext
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

func Test_WaitParsed(t *testing.T) {
	type fields struct {
		answers  []string
		worldErr error
	}
	errWorld := errors.New("Cannot find frame with given id")
	tests := []struct {
		name     string
		fields   fields
		deadline time.Duration
		wantErr  error
		// errText is checked where the error wraps nothing: an exception in the page is a new error.
		errText string
		assert  func(rt *fakeRuntime, worlds *fakeWorlds)
	}{
		{
			name:   "Should return at once when the document is parsed",
			fields: fields{answers: []string{"parsed"}},
			assert: func(rt *fakeRuntime, _ *fakeWorlds) {
				So(rt.calls, ShouldEqual, 1)
			},
		},
		{
			name:   "Should wait while the document is loading",
			fields: fields{answers: []string{"loading", "loading", "parsed"}},
			assert: func(rt *fakeRuntime, _ *fakeWorlds) {
				So(rt.calls, ShouldEqual, 3)
			},
		},
		{
			name:   "Should recreate the isolated world and repeat the call when the document changed between polls",
			fields: fields{answers: []string{"loading", "stale", "parsed"}},
			assert: func(rt *fakeRuntime, worlds *fakeWorlds) {
				So(worlds.calls, ShouldEqual, 1)
				So(rt.calls, ShouldEqual, 3)
			},
		},
		{
			// The retry is one per call and the wait ends on the first error.
			name:    "Should fail when the call and its retry both hit a changed document",
			fields:  fields{answers: []string{"stale", "stale", "parsed"}},
			wantErr: errStaleContext,
			assert: func(rt *fakeRuntime, worlds *fakeWorlds) {
				So(rt.calls, ShouldEqual, 2)
				So(worlds.calls, ShouldEqual, 1)
			},
		},
		{
			name:    "Should fail when the isolated world cannot be recreated",
			fields:  fields{answers: []string{"stale", "parsed"}, worldErr: errWorld},
			wantErr: errWorld,
			assert: func(rt *fakeRuntime, worlds *fakeWorlds) {
				So(rt.calls, ShouldEqual, 1)
				So(worlds.calls, ShouldEqual, 1)
			},
		},
		{
			name:     "Should return the context error when the call ends",
			fields:   fields{answers: []string{"loading"}},
			deadline: 50 * time.Millisecond,
			wantErr:  context.DeadlineExceeded,
			assert:   func(*fakeRuntime, *fakeWorlds) {},
		},
		{
			// An exception in the page comes as a reply with ExceptionDetails, not as a transport error.
			name:    "Should fail on an exception in the page",
			fields:  fields{answers: []string{"exception"}},
			errText: "TypeError: document is not defined",
			assert: func(rt *fakeRuntime, _ *fakeWorlds) {
				So(rt.calls, ShouldEqual, 1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				ec, rt, worlds := parsedRuntime(tt.fields.answers...)
				worlds.err = tt.fields.worldErr

				// A broken wait must fail the case, not hang the test.
				deadline := tt.deadline
				if deadline == 0 {
					deadline = 5 * time.Second
				}

				ctx, cancel := context.WithTimeout(context.Background(), deadline)
				t.Cleanup(cancel)

				err := waitParsed(ctx, ec, 10*time.Millisecond)

				switch {
				case tt.wantErr != nil:
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
				case tt.errText != "":
					So(err, ShouldNotBeNil)
					So(err.Error(), ShouldContainSubstring, tt.errText)
				default:
					So(err, ShouldBeNil)
				}

				tt.assert(rt, worlds)
			})
		})
	}
}

// The fake runtime answers only this condition, so the other tests check that waitParsed uses it.
func Test_DOMParsedTemplate(t *testing.T) {
	Convey("Should wait for any readyState but loading and never for complete", t, func() {
		src := templates.DOMParsed().String()

		So(src, ShouldContainSubstring, "document.readyState !== 'loading'")
		So(src, ShouldNotContainSubstring, "complete")
	})
}

func Test_PageLoadStepFor(t *testing.T) {
	urlVerdict := newLazyAttachReport(drivers.Params{})
	urlVerdict.setVerdict(lazyVerdict{Reason: verdictURL})
	titleVerdict := newLazyAttachReport(drivers.Params{})
	titleVerdict.setVerdict(lazyVerdict{Reason: verdictTitle})
	skipOnURL := &drivers.LazyAttach{WaitReadyOnURL: false}
	waitOnURL := &drivers.LazyAttach{WaitReadyOnURL: true}
	tests := []struct {
		name     string
		params   drivers.Params
		report   *lazyAttachReport
		wantStep pageLoadStep
		wantSkip bool
	}{
		{
			name:     "Should load a blank page as is",
			params:   drivers.Params{URL: BlankPageURL, DirectNavigation: true, LazyAttach: skipOnURL},
			report:   urlVerdict,
			wantStep: stepLoadBlank,
		},
		{
			name:     "Should load a page without a URL as blank",
			params:   drivers.Params{},
			wantStep: stepLoadBlank,
		},
		{
			name:     "Should navigate without directNavigation",
			params:   drivers.Params{URL: waitTestURL},
			wantStep: stepNavigate,
		},
		{
			name:     "Should take the direct step without lazyAttach",
			params:   drivers.Params{URL: waitTestURL, DirectNavigation: true},
			wantStep: stepDirect,
		},
		{
			name:     "Should skip the wait for parsing after a verdict by url without waitReadyOnURL",
			params:   drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL},
			report:   urlVerdict,
			wantStep: stepLazy,
			wantSkip: true,
		},
		{
			name:     "Should wait for parsing after a verdict by url with waitReadyOnURL",
			params:   drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: waitOnURL},
			report:   urlVerdict,
			wantStep: stepLazy,
		},
		{
			name:     "Should wait for parsing after a verdict by title",
			params:   drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL},
			report:   titleVerdict,
			wantStep: stepLazy,
		},
		{
			name:     "Should wait for parsing without a report",
			params:   drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: skipOnURL},
			wantStep: stepLazy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				step, skip := pageLoadStepFor(tt.params, tt.report)

				So(step, ShouldEqual, tt.wantStep)
				So(skip, ShouldEqual, tt.wantSkip)
			})
		})
	}
}
