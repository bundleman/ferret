package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/rs/zerolog"

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

// TestOpenPageIsolatedContextBeforeTab: the own context is created before the tab, the tab is
// opened in it, and the record of the call says isolated and incognito even when the driver
// keeps cookies.
func TestOpenPageIsolatedContextBeforeTab(t *testing.T) {
	var buf bytes.Buffer
	iso := &isolation{}

	connect := func(_ context.Context, _ drivers.Params, contextID browser.ContextID, report *lazyAttachReport) (*rpcc.Conn, error) {
		iso.step("connect:" + string(contextID))
		report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*"})

		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
		iso.step("load")
		report.setCommitted(true)
		report.setParsed("ready", time.Millisecond)

		return &HTMLPage{}, nil
	}

	page, err := NewDriver(WithKeepCookies()).openPageWith(openPageCtx(&buf), isolatedParams(), true, iso.isolate, connect, load)

	if err != nil || page == nil {
		t.Fatalf("got %v, %v", page, err)
	}

	if strings.Join(iso.steps, ",") != "isolate,connect:OWN,load" {
		t.Errorf("steps %v, want the own context before the tab and the tab in it", iso.steps)
	}

	recs := lazyRecords(t, &buf)

	if len(recs) != 1 || recs[0]["isolated"] != true || recs[0]["incognito"] != true {
		t.Errorf("want one isolated incognito record, got %v", recs)
	}
}

// TestOpenPageIsolatedSuccessHandsContextToPage: a call that succeeded leaves the context to its
// page, which disposes it when it is closed.
func TestOpenPageIsolatedSuccessHandsContextToPage(t *testing.T) {
	iso := &isolation{}
	page := &HTMLPage{}

	connect := func(_ context.Context, _ drivers.Params, _ browser.ContextID, _ *lazyAttachReport) (*rpcc.Conn, error) {
		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		return page, nil
	}

	got, err := NewDriver().openPageWith(context.Background(), isolatedParams(), true, iso.isolate, connect, load)

	if err != nil || got != page {
		t.Fatalf("got %v, %v", got, err)
	}

	if iso.count() != 0 || page.dispose == nil {
		t.Fatalf("the context must live on with the page: %d disposals, page dispose set %v", iso.count(), page.dispose != nil)
	}

	page.dispose()

	if iso.count() != 1 {
		t.Errorf("the page must hold the dispose of its own context, got %d disposals", iso.count())
	}
}

// TestOpenPageIsolatedDisposedOnFailure: a call that failed after the context was created
// disposes it at once, also when the failed load returns its page.
func TestOpenPageIsolatedDisposedOnFailure(t *testing.T) {
	failed := &HTMLPage{}
	loadErr := errors.New("load failed")

	cases := []struct {
		name     string
		connect  error
		wantPage *HTMLPage
	}{
		{"connect", errors.New("create a browser target"), nil},
		{"load", nil, failed},
	}

	for _, c := range cases {
		iso := &isolation{}

		connect := func(_ context.Context, _ drivers.Params, _ browser.ContextID, _ *lazyAttachReport) (*rpcc.Conn, error) {
			if c.connect != nil {
				return nil, c.connect
			}

			return &rpcc.Conn{}, nil
		}
		load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
			return failed, loadErr
		}

		page, err := NewDriver().openPageWith(context.Background(), isolatedParams(), false, iso.isolate, connect, load)

		if err == nil || page != c.wantPage {
			t.Errorf("%s: got %v, %v", c.name, page, err)
		}

		if iso.count() != 1 {
			t.Errorf("%s: want the context disposed once, got %d", c.name, iso.count())
		}

		if failed.dispose != nil {
			t.Errorf("%s: a failed page must not hold the context", c.name)
		}
	}
}

// TestOpenPageIsolatedCreateError: a call whose context could not be created opens no tab and is
// written as failed before the first poll.
func TestOpenPageIsolatedCreateError(t *testing.T) {
	var buf bytes.Buffer
	iso := &isolation{err: errors.New("create a browser context")}

	connect := func(_ context.Context, _ drivers.Params, _ browser.ContextID, _ *lazyAttachReport) (*rpcc.Conn, error) {
		t.Error("no tab without its context")

		return nil, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		t.Error("no load without a tab")

		return nil, nil
	}

	page, err := NewDriver().openPageWith(openPageCtx(&buf), isolatedParams(), true, iso.isolate, connect, load)

	if err == nil || page != nil {
		t.Fatalf("got %v, %v", page, err)
	}

	recs := lazyRecords(t, &buf)

	if len(recs) != 1 || recs[0]["verdict"] != "before_poll" || recs[0]["stage"] != "prepare" || recs[0]["isolated"] != true {
		t.Errorf("want one before_poll record of the prepare stage, got %v", recs)
	}
}

// TestOpenPageWithoutIsolatedContext: a call without the param creates no context and opens the
// tab in the context of the driver.
func TestOpenPageWithoutIsolatedContext(t *testing.T) {
	var buf bytes.Buffer
	iso := &isolation{}

	connect := func(_ context.Context, _ drivers.Params, contextID browser.ContextID, _ *lazyAttachReport) (*rpcc.Conn, error) {
		if contextID != "" {
			t.Errorf("a call without isolatedContext opens the tab in the driver context, got %q", contextID)
		}

		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		return &HTMLPage{}, nil
	}

	page, err := NewDriver().openPageWith(openPageCtx(&buf), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), true, iso.isolate, connect, load)

	if err != nil || page.dispose != nil || len(iso.steps) != 0 {
		t.Errorf("got %v, %v, dispose set %v, steps %v", page, err, page != nil && page.dispose != nil, iso.steps)
	}

	if recs := lazyRecords(t, &buf); len(recs) != 1 || recs[0]["isolated"] != false {
		t.Errorf("want one record with isolated false, got %v", recs)
	}
}

// TestOpenerOfIsolatedContext: the tab of an isolatedContext call goes to its own context and is
// incognito whatever the driver options are; other tabs keep the context of the driver.
func TestOpenerOfIsolatedContext(t *testing.T) {
	drv := NewDriver(WithKeepCookies())
	drv.client = &cdp.Client{}
	drv.contextID = "DRV"

	if o := drv.opener("OWN"); o.contextID != "OWN" || o.keepCookies {
		t.Errorf("isolated opener %+v, want the own context and no keepCookies", o)
	}

	if o := drv.opener(""); o.contextID != "DRV" || !o.keepCookies {
		t.Errorf("ordinary opener %+v, want the driver context and its keepCookies", o)
	}
}

// TestOpenRejectsIsolatedContextCombinations: a Go caller bypasses the DOCUMENT() parser, so the
// driver itself refuses isolatedContext with keepCookies or fetchMode, before any browser call.
func TestOpenRejectsIsolatedContextCombinations(t *testing.T) {
	drv := NewDriver(WithAddress("http://127.0.0.1:1"))
	defer drv.Close()

	for _, params := range []drivers.Params{
		{URL: waitTestURL, IsolatedContext: true, KeepCookies: true},
		{URL: waitTestURL, IsolatedContext: true, Fetch: &drivers.FetchParams{}},
	} {
		page, err := drv.Open(context.Background(), params)

		if page != nil || err == nil || !strings.HasPrefix(err.Error(), "invalid argument: isolatedContext cannot be used with") {
			t.Errorf("params %+v: want an invalid argument error, got %v, %v", params, page, err)
		}
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

// TestIsolateCreatesDisposableContext: the context is created to be disposed by Chrome when the
// driver connection drops, and the returned function disposes that very context.
func TestIsolateCreatesDisposableContext(t *testing.T) {
	targets := &contextTarget{}
	drv := NewDriver()
	// an initialized driver: isolate does not connect again
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: targets}

	id, dispose, err := drv.isolate(context.Background(), isolatedParams())

	if err != nil || id != "OWN" {
		t.Fatalf("got %q, %v", id, err)
	}

	if len(targets.created) != 1 || targets.created[0].DisposeOnDetach == nil || !*targets.created[0].DisposeOnDetach {
		t.Errorf("want one context created with disposeOnDetach, got %+v", targets.created)
	}

	if len(targets.disposed) != 0 {
		t.Fatalf("the context must live until dispose is called, got %v", targets.disposed)
	}

	dispose()

	if len(targets.disposed) != 1 || targets.disposed[0] != "OWN" {
		t.Errorf("want the own context disposed, got %v", targets.disposed)
	}
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

func TestDisposeContext(t *testing.T) {
	cases := []struct {
		name string
		err  error
		warn bool
	}{
		{"ok", nil, false},
		{"failed", errors.New("Target.disposeBrowserContext: no such context"), true},
		// the driver connection is gone: Chrome disposes the context itself (disposeOnDetach)
		{"connection closing", rpcc.ErrConnClosing, false},
		{"same text without the closed type", errors.New("x: " + rpcc.ErrConnClosing.Error()), true},
	}

	for _, c := range cases {
		var buf bytes.Buffer
		f := &fakeDisposer{err: c.err}

		disposeContext(zerolog.New(&buf), f, "OWN", waitTestURL)

		if len(f.ids) != 1 || f.ids[0] != "OWN" || !f.deadline {
			t.Errorf("%s: want one dispose of OWN on a context with a deadline, got %v, deadline %v", c.name, f.ids, f.deadline)
		}

		got := buf.String()

		if c.warn != (got != "") {
			t.Errorf("%s: warn %v, got %q", c.name, c.warn, got)
		}

		if c.warn {
			var rec map[string]interface{}

			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}

			if rec["level"] != "warn" || rec["browser_context_id"] != "OWN" || rec["url"] != waitTestURL || rec["error"] == nil {
				t.Errorf("%s: record %v", c.name, rec)
			}
		}
	}
}

// TestDisposeContextRecognizesWrappedClose: a closed connection is recognized also when it comes
// wrapped, as the Target domain client returns it.
func TestDisposeContextRecognizesWrappedClose(t *testing.T) {
	var buf bytes.Buffer

	disposeContext(zerolog.New(&buf), &fakeDisposer{err: wrapClosed(rpcc.ErrConnClosing)}, "OWN", waitTestURL)

	if buf.Len() != 0 {
		t.Errorf("no warn after the connection closed, got %q", buf.String())
	}
}

type wrappedErr struct{ err error }

func (w *wrappedErr) Error() string { return "Target.disposeBrowserContext: " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }

func wrapClosed(err error) error { return &wrappedErr{err: err} }

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
func pipeConn(t *testing.T) *rpcc.Conn {
	t.Helper()

	conn, err := rpcc.DialContext(context.Background(), "pipe", rpcc.WithDialer(func(context.Context, string) (io.ReadWriteCloser, error) {
		c, _ := net.Pipe()

		return c, nil
	}))

	if err != nil {
		t.Fatal(err)
	}

	return conn
}

// TestDestroyDisposesContextAfterTab: an isolatedContext page disposes its context after the
// tab is closed, also when closing the tab failed, and only once.
func TestDestroyDisposesContextAfterTab(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("page is on Chrome's error page")} {
		var steps []string
		client := &cdp.Client{Page: &closingPage{steps: &steps, err: closeErr}, Target: &closingTarget{steps: &steps}}
		domManager, err := dom.New(zerolog.Nop(), client, nil, nil)

		if err != nil {
			t.Fatal(err)
		}

		page := &HTMLPage{
			logger:  zerolog.Nop(),
			conn:    pipeConn(t),
			client:  client,
			dom:     domManager,
			network: &net2.Manager{},
		}
		page.setDispose(func() { steps = append(steps, "dispose") })

		if err := page.Close(); err != nil {
			t.Fatal(err)
		}

		_ = page.Close()

		if steps[len(steps)-1] != "dispose" || strings.Count(strings.Join(steps, ","), "dispose") != 1 || steps[0] != "page.close" {
			t.Errorf("close error %v: steps %v, want the dispose once, after closing the tab", closeErr, steps)
		}
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

// TestCreateConnectionPutsTabAndCookiesInContext: createConnection opens the tab of an
// isolatedContext call, and sets its cookies, in the own context, and the tab of another call in
// the context of the driver. The poll fails, so the call ends before it attaches to the tab.
func TestCreateConnectionPutsTabAndCookiesInContext(t *testing.T) {
	cases := []struct {
		contextID browser.ContextID
		want      browser.ContextID
	}{
		{"OWN", "OWN"},
		{"", "DRV"},
	}

	for _, c := range cases {
		targets := &fakeTargets{tab: &scriptedTab{err: errors.New("poll failed")}}
		cookies := &fakeCookies{}
		drv := NewDriver()
		// an initialized driver: createConnection does not connect again
		drv.session = &session.Manager{}
		drv.client = &cdp.Client{Target: &cdpTargets{f: targets}, Storage: &cdpStorage{f: cookies}}
		drv.contextID = "DRV"

		params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
		params.Cookies = drivers.NewHTTPCookiesWith(map[string]drivers.HTTPCookie{"a": {Name: "a", Value: "1"}})

		if _, err := drv.createConnection(context.Background(), params, c.contextID, newLazyAttachReport(params)); err == nil {
			t.Fatalf("context %q: want the poll error", c.contextID)
		}

		if len(targets.created) != 1 || targets.created[0].BrowserContextID == nil || *targets.created[0].BrowserContextID != c.want {
			t.Errorf("context %q: the tab must go to %q, got %+v", c.contextID, c.want, targets.created)
		}

		if len(cookies.set) != 1 || cookies.set[0].BrowserContextID == nil || *cookies.set[0].BrowserContextID != c.want {
			t.Errorf("context %q: the cookies must go to %q, got %+v", c.contextID, c.want, cookies.set)
		}
	}
}

// TestOpenPageIsolatedWithoutLazyAttach: the param acts without lazyAttach too.
func TestOpenPageIsolatedWithoutLazyAttach(t *testing.T) {
	iso := &isolation{}

	connect := func(_ context.Context, _ drivers.Params, contextID browser.ContextID, _ *lazyAttachReport) (*rpcc.Conn, error) {
		iso.step("connect:" + string(contextID))

		return nil, errors.New("create a browser target")
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		return nil, nil
	}

	_, err := NewDriver().openPageWith(context.Background(), drivers.Params{URL: waitTestURL, IsolatedContext: true}, false, iso.isolate, connect, load)

	if err == nil || strings.Join(iso.steps, ",") != "isolate,connect:OWN,dispose" {
		t.Errorf("got %v, steps %v, want the own context created, used and disposed", err, iso.steps)
	}
}

// TestIsolateInitializesDriver: isolate on a fresh driver connects first instead of using the
// missing client.
func TestIsolateInitializesDriver(t *testing.T) {
	drv := NewDriver(WithAddress("http://127.0.0.1:1"))
	defer drv.Close()

	_, _, err := drv.isolate(context.Background(), isolatedParams())

	if err == nil || !strings.Contains(err.Error(), "initialize driver") {
		t.Errorf("want the error of initializing the driver, got %v", err)
	}
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

// TestIsolateCanceledLeavesNoContext: a call canceled while its context is being created gets the
// cancel error and does not leave the context in the browser; the request itself is bounded by
// the close timeout, since the call's own deadline no longer applies to it.
func TestIsolateCanceledLeavesNoContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targets := &cancelingTarget{cancel: cancel}
	drv := NewDriver()
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: targets}

	_, _, err := drv.isolate(ctx, isolatedParams())

	if !errors.Is(err, context.Canceled) {
		t.Errorf("want the cancel error, got %v", err)
	}

	if len(targets.created) != 1 || len(targets.disposed) != 1 || targets.disposed[0] != "OWN" {
		t.Errorf("created %d contexts, disposed %v, want the created one disposed", len(targets.created), targets.disposed)
	}

	if targets.deadline <= 0 || targets.deadline > pageCloseTimeout {
		t.Errorf("the create request must be bounded by pageCloseTimeout, got deadline in %v", targets.deadline)
	}
}

// failingDisposeTarget creates contexts and fails to dispose them.
type failingDisposeTarget struct {
	contextTarget
}

func (f *failingDisposeTarget) DisposeBrowserContext(ctx context.Context, args *target.DisposeBrowserContextArgs) error {
	_ = f.contextTarget.DisposeBrowserContext(ctx, args)

	return errors.New("Target.disposeBrowserContext: no such context")
}

// TestIsolateDisposeWarnsWithURL: the dispose returned by isolate writes its failure to the log of
// the call with the context id and the requested URL.
func TestIsolateDisposeWarnsWithURL(t *testing.T) {
	var buf bytes.Buffer
	drv := NewDriver()
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: &failingDisposeTarget{}}

	_, dispose, err := drv.isolate(openPageCtx(&buf), isolatedParams())

	if err != nil {
		t.Fatal(err)
	}

	dispose()

	out := buf.String()

	if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, `"browser_context_id":"OWN"`) ||
		!strings.Contains(out, `"url":"`+waitTestURL+`"`) {
		t.Errorf("want a warn record with the context id and the URL, got %q", out)
	}
}

// closedClient is a Target client over a connection the browser side has closed, as after a
// browser crash: its calls fail with "the connection is closing" and the cause.
func closedClient(t *testing.T) cdp.Target {
	t.Helper()

	client, server := net.Pipe()
	conn, err := rpcc.DialContext(context.Background(), "pipe", rpcc.WithDialer(func(context.Context, string) (io.ReadWriteCloser, error) {
		return client, nil
	}))

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	_ = server.Close()
	<-conn.Context().Done()

	return cdp.NewClient(conn).Target
}

// TestDisposeContextClosedByBrowser: a connection closed with a cause, as the Target client
// returns it, writes no warn either.
func TestDisposeContextClosedByBrowser(t *testing.T) {
	var buf bytes.Buffer

	disposeContext(zerolog.New(&buf), closedClient(t), "OWN", waitTestURL)

	if buf.Len() != 0 {
		t.Errorf("no warn after the browser closed the connection, got %q", buf.String())
	}
}

// TestIsolateKeepsConnectionClosingText: the downloader closes its driver on the text "the
// connection is closing", so isolate must not lose it.
func TestIsolateKeepsConnectionClosingText(t *testing.T) {
	drv := NewDriver()
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: closedClient(t)}

	_, _, err := drv.isolate(context.Background(), isolatedParams())

	if err == nil || !strings.Contains(err.Error(), "the connection is closing") {
		t.Errorf("want the connection closing text, got %v", err)
	}
}

// TestIsolateAlreadyCanceledCreatesNothing: a call that comes already canceled does not ask the
// browser for a context at all.
func TestIsolateAlreadyCanceledCreatesNothing(t *testing.T) {
	targets := &contextTarget{}
	drv := NewDriver()
	drv.session = &session.Manager{}
	drv.client = &cdp.Client{Target: targets}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := drv.isolate(ctx, isolatedParams())

	if !errors.Is(err, context.Canceled) || len(targets.created) != 0 || len(targets.disposed) != 0 {
		t.Errorf("got %v, created %d, disposed %v, want the cancel error and no browser call", err, len(targets.created), targets.disposed)
	}
}
