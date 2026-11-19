package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/browser"
	"github.com/mafredri/cdp/protocol/storage"
	"github.com/mafredri/cdp/protocol/target"
	"github.com/mafredri/cdp/rpcc"
	"github.com/mafredri/cdp/session"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/dom"
	net2 "github.com/MontFerret/ferret/pkg/drivers/cdp/network"
)

// isolation records what openPageWith did with the context of an isolatedContext call.
type isolation struct {
	mu        sync.Mutex
	steps     []string
	err       error
	disposals int
}

func (s *isolation) step(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.steps = append(s.steps, name)
}

func (s *isolation) isolate(_ context.Context, _ drivers.Params) (browser.ContextID, func(), error) {
	s.step("isolate")

	if s.err != nil {
		return "", nil, s.err
	}

	return "OWN", func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.disposals++
		s.steps = append(s.steps, "dispose")
	}, nil
}

func (s *isolation) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.disposals
}

func isolatedParams() drivers.Params {
	params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	params.IsolatedContext = true

	return params
}

func Test_Driver_OpenPageWith_IsolatedContext(t *testing.T) {
	type fields struct {
		driverOpts []Option
		isolateErr error
		connect    func(report *lazyAttachReport) (*rpcc.Conn, error)
		load       func(params drivers.Params, report *lazyAttachReport) (*HTMLPage, error)
	}
	type args struct {
		params     drivers.Params
		logConnErr bool
	}
	errTarget := errors.New("create a browser target")
	errLoad := errors.New("load failed")
	errCreate := errors.New("create a browser context")
	handed := &HTMLPage{}
	failedOnConnect, failedOnLoad := &HTMLPage{}, &HTMLPage{}
	connected := func(*lazyAttachReport) (*rpcc.Conn, error) { return &rpcc.Conn{}, nil }
	byTitle := func(report *lazyAttachReport) (*rpcc.Conn, error) {
		report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*"})

		return &rpcc.Conn{}, nil
	}
	parsed := func(_ drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
		report.setCommitted(true)
		report.setParsed("ready", time.Millisecond)

		return &HTMLPage{}, nil
	}
	failLoad := func(page *HTMLPage) func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) {
		return func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return page, errLoad }
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr error
		assert  func(page *HTMLPage, iso *isolation, log *bytes.Buffer)
	}{
		{
			// The record says isolated and incognito even when the driver keeps cookies.
			name: "Should create the own context before the tab and open the tab in it",
			fields: fields{
				driverOpts: []Option{WithKeepCookies()},
				connect:    byTitle,
				load:       parsed,
			},
			args: args{params: isolatedParams(), logConnErr: true},
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page, ShouldNotBeNil)
				So(iso.steps, ShouldResemble, []string{"isolate", "connect:OWN", "load"})

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["isolated"], ShouldEqual, true)
				So(recs[0]["incognito"], ShouldEqual, true)
			},
		},
		{
			name: "Should leave the context to the page of a call that succeeded",
			fields: fields{
				connect: connected,
				load:    func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return handed, nil },
			},
			args: args{params: isolatedParams(), logConnErr: true},
			assert: func(page *HTMLPage, iso *isolation, _ *bytes.Buffer) {
				So(page, ShouldPointTo, handed)
				So(iso.count(), ShouldEqual, 0)
				So(handed.dispose, ShouldNotBeNil)

				handed.dispose()

				So(iso.count(), ShouldEqual, 1)
			},
		},
		{
			name: "Should open the tab in the driver context without the param",
			fields: fields{
				connect: connected,
				load:    func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return &HTMLPage{}, nil },
			},
			args: args{params: lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), logConnErr: true},
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page.dispose, ShouldBeNil)
				So(iso.steps, ShouldResemble, []string{"connect:", "load"})

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["isolated"], ShouldEqual, false)
			},
		},
		{
			name: "Should dispose the context at once when the tab cannot be opened",
			fields: fields{
				connect: func(*lazyAttachReport) (*rpcc.Conn, error) { return nil, errTarget },
				load:    failLoad(failedOnConnect),
			},
			args:    args{params: isolatedParams()},
			wantErr: errTarget,
			assert: func(page *HTMLPage, iso *isolation, _ *bytes.Buffer) {
				So(page, ShouldBeNil)
				So(iso.count(), ShouldEqual, 1)
				So(failedOnConnect.dispose, ShouldBeNil)
			},
		},
		{
			name: "Should dispose the context at once when the load fails and return its page",
			fields: fields{
				connect: connected,
				load:    failLoad(failedOnLoad),
			},
			args:    args{params: isolatedParams()},
			wantErr: errLoad,
			assert: func(page *HTMLPage, iso *isolation, _ *bytes.Buffer) {
				So(page, ShouldPointTo, failedOnLoad)
				So(iso.count(), ShouldEqual, 1)
				So(failedOnLoad.dispose, ShouldBeNil)
			},
		},
		{
			name: "Should open no tab and write a failure before the first poll when the context cannot be created",
			fields: fields{
				isolateErr: errCreate,
				connect:    connected,
				load:       parsed,
			},
			args:    args{params: isolatedParams(), logConnErr: true},
			wantErr: errCreate,
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page, ShouldBeNil)
				So(iso.steps, ShouldResemble, []string{"isolate"})

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["verdict"], ShouldEqual, "before_poll")
				So(recs[0]["stage"], ShouldEqual, "prepare")
				So(recs[0]["isolated"], ShouldEqual, true)
			},
		},
		{
			name: "Should create, use and dispose the own context without lazyAttach",
			fields: fields{
				connect: func(*lazyAttachReport) (*rpcc.Conn, error) { return nil, errTarget },
				load:    func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return nil, nil },
			},
			args:    args{params: drivers.Params{URL: waitTestURL, IsolatedContext: true}},
			wantErr: errTarget,
			assert: func(_ *HTMLPage, iso *isolation, _ *bytes.Buffer) {
				So(iso.steps, ShouldResemble, []string{"isolate", "connect:OWN", "dispose"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var log bytes.Buffer

				iso := &isolation{err: tt.fields.isolateErr}
				connect, load := openPageFakes(iso, tt.fields.connect, tt.fields.load)

				page, err := NewDriver(tt.fields.driverOpts...).openPageWith(openPageCtx(&log), tt.args.params, tt.args.logConnErr, iso.isolate, connect, load)

				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
				} else {
					So(err, ShouldBeNil)
				}

				tt.assert(page, iso, &log)
			})
		})
	}
}

// The tab of an isolatedContext call is incognito whatever the driver options are.
func Test_Driver_Opener(t *testing.T) {
	tests := []struct {
		name            string
		contextID       browser.ContextID
		wantContextID   browser.ContextID
		wantKeepCookies bool
	}{
		{
			name:            "Should open the tab of an isolatedContext call in its own context without keepCookies",
			contextID:       "OWN",
			wantContextID:   "OWN",
			wantKeepCookies: false,
		},
		{
			name:            "Should open other tabs in the driver context with its keepCookies",
			contextID:       "",
			wantContextID:   "DRV",
			wantKeepCookies: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				drv := NewDriver(WithKeepCookies())
				drv.client = &cdp.Client{}
				drv.contextID = "DRV"

				o := drv.opener(tt.contextID)

				So(o.contextID, ShouldEqual, tt.wantContextID)
				So(o.keepCookies, ShouldEqual, tt.wantKeepCookies)
			})
		})
	}
}

// A Go caller bypasses the DOCUMENT() parser, so the driver itself refuses these combinations
// before any browser call.
func Test_Driver_Open_RejectsIsolatedContextCombinations(t *testing.T) {
	tests := []struct {
		name   string
		params drivers.Params
	}{
		{
			name:   "Should refuse isolatedContext with keepCookies",
			params: drivers.Params{URL: waitTestURL, IsolatedContext: true, KeepCookies: true},
		},
		{
			name:   "Should refuse isolatedContext with fetchMode",
			params: drivers.Params{URL: waitTestURL, IsolatedContext: true, Fetch: &drivers.FetchParams{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				drv := NewDriver(WithAddress("http://127.0.0.1:1"))
				t.Cleanup(func() { _ = drv.Close() })

				page, err := drv.Open(context.Background(), tt.params)

				// Open returns an interface: a typed nil pointer in it would pass ShouldBeNil.
				So(page == nil, ShouldBeTrue)
				So(err, ShouldNotBeNil)
				// core.Error does not wrap ErrInvalidArgument, so the error is told by its text
				So(err.Error(), ShouldStartWith, "invalid argument: isolatedContext cannot be used with")
			})
		})
	}
}

// contextTarget answers the context calls of isolate and records them.
type contextTarget struct {
	cdp.Target
	created  []*target.CreateBrowserContextArgs
	disposed []browser.ContextID
}

func (c *contextTarget) CreateBrowserContext(_ context.Context, args *target.CreateBrowserContextArgs) (*target.CreateBrowserContextReply, error) {
	c.created = append(c.created, args)

	return &target.CreateBrowserContextReply{BrowserContextID: "OWN"}, nil
}

func (c *contextTarget) DisposeBrowserContext(_ context.Context, args *target.DisposeBrowserContextArgs) error {
	c.disposed = append(c.disposed, args.BrowserContextID)

	return nil
}

// cancelingTarget plays a browser that gets the create request, after which the call is canceled
// before the answer arrives. It records whether the request ran on a context with a deadline.
type cancelingTarget struct {
	contextTarget
	cancel   context.CancelFunc
	deadline time.Duration
}

func (c *cancelingTarget) CreateBrowserContext(ctx context.Context, args *target.CreateBrowserContextArgs) (*target.CreateBrowserContextReply, error) {
	reply, _ := c.contextTarget.CreateBrowserContext(ctx, args)
	c.cancel()

	if d, ok := ctx.Deadline(); ok {
		c.deadline = time.Until(d)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return reply, nil
}

// failingDisposeTarget creates contexts and fails to dispose them.
type failingDisposeTarget struct {
	contextTarget
}

func (f *failingDisposeTarget) DisposeBrowserContext(ctx context.Context, args *target.DisposeBrowserContextArgs) error {
	_ = f.contextTarget.DisposeBrowserContext(ctx, args)

	return errors.New("Target.disposeBrowserContext: no such context")
}

// initializedDriver returns a driver that isolate and createConnection do not connect again.
func initializedDriver(targets cdp.Target) *Driver {
	drv := NewDriver()
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: targets}

	return drv
}

func Test_Driver_Isolate(t *testing.T) {
	Convey("Driver.isolate", t, func() {
		// Chrome disposes such a context when the driver connection drops.
		Convey("Should create a context with disposeOnDetach that lives until dispose is called", func() {
			targets := &contextTarget{}

			id, dispose, err := initializedDriver(targets).isolate(context.Background(), isolatedParams())
			So(err, ShouldBeNil)
			So(id, ShouldEqual, "OWN")
			So(len(targets.created), ShouldEqual, 1)
			So(targets.created[0].DisposeOnDetach, ShouldNotBeNil)
			So(*targets.created[0].DisposeOnDetach, ShouldBeTrue)
			So(len(targets.disposed), ShouldEqual, 0)

			dispose()

			So(targets.disposed, ShouldResemble, []browser.ContextID{"OWN"})
		})

		Convey("Should connect a fresh driver first instead of using the missing client", func() {
			drv := NewDriver(WithAddress("http://127.0.0.1:1"))
			t.Cleanup(func() { _ = drv.Close() })

			_, _, err := drv.isolate(context.Background(), isolatedParams())
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "initialize driver")
		})

		// The request is bounded by the close timeout, since the deadline of the call no longer applies to it.
		Convey("Should dispose the context of a call canceled while it was being created", func() {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			targets := &cancelingTarget{cancel: cancel}

			_, _, err := initializedDriver(targets).isolate(ctx, isolatedParams())
			So(errors.Is(err, context.Canceled), ShouldBeTrue)
			So(len(targets.created), ShouldEqual, 1)
			So(targets.disposed, ShouldResemble, []browser.ContextID{"OWN"})
			So(targets.deadline, ShouldBeGreaterThan, 0)
			So(targets.deadline, ShouldBeLessThanOrEqualTo, pageCloseTimeout)
		})

		Convey("Should not ask for a context when the call comes already canceled", func() {
			targets := &contextTarget{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, _, err := initializedDriver(targets).isolate(ctx, isolatedParams())
			So(errors.Is(err, context.Canceled), ShouldBeTrue)
			So(len(targets.created), ShouldEqual, 0)
			So(len(targets.disposed), ShouldEqual, 0)
		})

		Convey("Should write a failed dispose to the log of the call with the context id and the URL", func() {
			var buf bytes.Buffer

			_, dispose, err := initializedDriver(&failingDisposeTarget{}).isolate(openPageCtx(&buf), isolatedParams())
			So(err, ShouldBeNil)

			dispose()

			out := buf.String()
			So(out, ShouldContainSubstring, `"level":"warn"`)
			So(out, ShouldContainSubstring, `"browser_context_id":"OWN"`)
			So(out, ShouldContainSubstring, `"url":"`+waitTestURL+`"`)
		})

		// The downloader closes its driver on this text.
		Convey("Should keep the text of a closed connection", func() {
			_, _, err := initializedDriver(closedClient(t)).isolate(context.Background(), isolatedParams())
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "the connection is closing")
		})
	})
}

type fakeDisposer struct {
	err      error
	ids      []browser.ContextID
	deadline bool
}

func (f *fakeDisposer) DisposeBrowserContext(ctx context.Context, args *target.DisposeBrowserContextArgs) error {
	_, f.deadline = ctx.Deadline()
	f.ids = append(f.ids, args.BrowserContextID)

	return f.err
}

type wrappedErr struct{ err error }

func (w *wrappedErr) Error() string { return "Target.disposeBrowserContext: " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }

// closedClient is a Target client over a connection the browser side has closed, as after a
// browser crash: its calls fail with "the connection is closing" and the cause.
func closedClient(t *testing.T) cdp.Target {
	client, server := net.Pipe()
	conn, err := rpcc.DialContext(context.Background(), "pipe", rpcc.WithDialer(func(context.Context, string) (io.ReadWriteCloser, error) {
		return client, nil
	}))
	So(err, ShouldBeNil)

	t.Cleanup(func() { _ = conn.Close() })

	_ = server.Close()
	<-conn.Context().Done()

	return cdp.NewClient(conn).Target
}

func Test_DisposeContext(t *testing.T) {
	tests := []struct {
		name string
		// fake is set when the call itself is checked: one dispose of OWN on a context with a deadline
		fake     *fakeDisposer
		disposer func(t *testing.T) contextDisposer
		warn     bool
	}{
		{
			name: "Should write nothing when the context is disposed",
			fake: &fakeDisposer{},
			warn: false,
		},
		{
			name: "Should write a warn record when the dispose fails",
			fake: &fakeDisposer{err: errors.New("Target.disposeBrowserContext: no such context")},
			warn: true,
		},
		{
			// Chrome disposes the context itself when the driver connection is gone (disposeOnDetach).
			name: "Should write nothing when the connection is closing",
			fake: &fakeDisposer{err: rpcc.ErrConnClosing},
			warn: false,
		},
		{
			name: "Should write a warn record for the same text without the closed type",
			fake: &fakeDisposer{err: errors.New("x: " + rpcc.ErrConnClosing.Error())},
			warn: true,
		},
		{
			name: "Should recognize a closed connection that comes wrapped, as the Target client returns it",
			disposer: func(*testing.T) contextDisposer {
				return &fakeDisposer{err: &wrappedErr{err: rpcc.ErrConnClosing}}
			},
			warn: false,
		},
		{
			name:     "Should write nothing after the browser closed the connection",
			disposer: func(t *testing.T) contextDisposer { return closedClient(t) },
			warn:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var buf bytes.Buffer

				var disposer contextDisposer = tt.fake

				if tt.disposer != nil {
					disposer = tt.disposer(t)
				}

				disposeContext(zerolog.New(&buf), disposer, "OWN", waitTestURL)

				if tt.fake != nil {
					So(tt.fake.ids, ShouldResemble, []browser.ContextID{"OWN"})
					So(tt.fake.deadline, ShouldBeTrue)
				}

				So(buf.Len() != 0, ShouldEqual, tt.warn)

				if tt.warn {
					var rec map[string]interface{}

					So(json.Unmarshal(buf.Bytes(), &rec), ShouldBeNil)
					So(rec["level"], ShouldEqual, "warn")
					So(rec["browser_context_id"], ShouldEqual, "OWN")
					So(rec["url"], ShouldEqual, waitTestURL)
					So(rec["error"], ShouldNotBeNil)
				}
			})
		})
	}
}

// closingPage answers Page.close for destroy and records it.
type closingPage struct {
	cdp.Page
	steps *[]string
	err   error
}

func (p *closingPage) Close(context.Context) error {
	*p.steps = append(*p.steps, "page.close")

	return p.err
}

type closingTarget struct {
	cdp.Target
	steps *[]string
}

func (t *closingTarget) GetTargetInfo(context.Context, *target.GetTargetInfoArgs) (*target.GetTargetInfoReply, error) {
	*t.steps = append(*t.steps, "target.info")

	return nil, errors.New("target is gone")
}

// pipeConn is a live rpcc connection over a pipe, so destroy can close it.
func pipeConn() *rpcc.Conn {
	conn, err := rpcc.DialContext(context.Background(), "pipe", rpcc.WithDialer(func(context.Context, string) (io.ReadWriteCloser, error) {
		c, _ := net.Pipe()

		return c, nil
	}))
	So(err, ShouldBeNil)

	return conn
}

func Test_HTMLPage_Destroy(t *testing.T) {
	tests := []struct {
		name     string
		closeErr error
	}{
		{
			name:     "Should dispose the context once, after closing the tab",
			closeErr: nil,
		},
		{
			name:     "Should dispose the context once, also when closing the tab failed",
			closeErr: errors.New("page is on Chrome's error page"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var steps []string
				client := &cdp.Client{Page: &closingPage{steps: &steps, err: tt.closeErr}, Target: &closingTarget{steps: &steps}}
				domManager, err := dom.New(zerolog.Nop(), client, nil, nil)
				So(err, ShouldBeNil)

				page := &HTMLPage{
					logger:  zerolog.Nop(),
					conn:    pipeConn(),
					client:  client,
					dom:     domManager,
					network: &net2.Manager{},
				}
				page.setDispose(func() { steps = append(steps, "dispose") })

				So(page.Close(), ShouldBeNil)

				_ = page.Close()

				So(steps[len(steps)-1], ShouldEqual, "dispose")
				So(strings.Count(strings.Join(steps, ","), "dispose"), ShouldEqual, 1)
				So(steps[0], ShouldEqual, "page.close")
			})
		})
	}
}

// cdpTargets lets the fakeTargets of the open tests stand for the Target domain of the client.
type cdpTargets struct {
	cdp.Target
	f *fakeTargets
}

func (c *cdpTargets) CreateTarget(ctx context.Context, args *target.CreateTargetArgs) (*target.CreateTargetReply, error) {
	return c.f.CreateTarget(ctx, args)
}

func (c *cdpTargets) GetTargetInfo(ctx context.Context, args *target.GetTargetInfoArgs) (*target.GetTargetInfoReply, error) {
	return c.f.GetTargetInfo(ctx, args)
}

func (c *cdpTargets) CloseTarget(ctx context.Context, args *target.CloseTargetArgs) (*target.CloseTargetReply, error) {
	return c.f.CloseTarget(ctx, args)
}

type cdpStorage struct {
	cdp.Storage
	f *fakeCookies
}

func (c *cdpStorage) SetCookies(ctx context.Context, args *storage.SetCookiesArgs) error {
	return c.f.SetCookies(ctx, args)
}

// The poll fails, so the call ends before it attaches to the tab.
func Test_Driver_CreateConnection(t *testing.T) {
	errPoll := errors.New("poll failed")
	tests := []struct {
		name      string
		contextID browser.ContextID
		want      browser.ContextID
	}{
		{
			name:      "Should put the tab and the cookies of an isolatedContext call in its own context",
			contextID: "OWN",
			want:      "OWN",
		},
		{
			name:      "Should put the tab and the cookies of another call in the driver context",
			contextID: "",
			want:      "DRV",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				targets := &fakeTargets{tab: &scriptedTab{err: errPoll}}
				cookies := &fakeCookies{}
				drv := initializedDriver(&cdpTargets{f: targets})
				drv.client.Storage = &cdpStorage{f: cookies}
				drv.contextID = "DRV"

				params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
				params.Cookies = drivers.NewHTTPCookiesWith(map[string]drivers.HTTPCookie{"a": {Name: "a", Value: "1"}})

				_, err := drv.createConnection(context.Background(), params, tt.contextID, newLazyAttachReport(params))
				So(errors.Is(err, errPoll), ShouldBeTrue)

				So(len(targets.created), ShouldEqual, 1)
				So(targets.created[0].BrowserContextID, ShouldNotBeNil)
				So(*targets.created[0].BrowserContextID, ShouldEqual, tt.want)
				So(len(cookies.set), ShouldEqual, 1)
				So(cookies.set[0].BrowserContextID, ShouldNotBeNil)
				So(*cookies.set[0].BrowserContextID, ShouldEqual, tt.want)
			})
		})
	}
}
