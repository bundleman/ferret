package cdp

import (
	"context"
	"sync"
	"time"

	"github.com/MontFerret/ferret/pkg/runtime/core"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/protocol/browser"
	"github.com/mafredri/cdp/protocol/network"
	"github.com/mafredri/cdp/protocol/storage"
	"github.com/mafredri/cdp/protocol/target"
	"github.com/mafredri/cdp/rpcc"
	"github.com/mafredri/cdp/session"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"

	"github.com/MontFerret/ferret/pkg/drivers"
	net "github.com/MontFerret/ferret/pkg/drivers/cdp/network"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

const DriverName = "cdp"
const BlankPageURL = "about:blank"

var defaultViewport = &drivers.Viewport{
	Width:  1600,
	Height: 900,
}

type Driver struct {
	mu        sync.Mutex
	dev       *devtool.DevTools
	conn      *rpcc.Conn
	client    *cdp.Client
	session   *session.Manager
	contextID browser.ContextID
	options   *Options

	fetchMu     sync.Mutex
	fetchPools  map[string]*fetchPool
	fetchStop   chan struct{}
	fetchClosed bool
}

func NewDriver(opts ...Option) *Driver {
	drv := new(Driver)
	drv.options = NewOptions(opts)
	drv.dev = devtool.New(drv.options.Address)

	return drv
}

func (drv *Driver) Name() string {
	return drv.options.Name
}

func (drv *Driver) Open(ctx context.Context, params drivers.Params) (drivers.HTMLPage, error) {
	// A Go caller bypasses the DOCUMENT() parser, which refuses these combinations too.
	if params.IsolatedContext && params.KeepCookies {
		return nil, core.Error(core.ErrInvalidArgument, "isolatedContext cannot be used with keepCookies: true")
	}

	if params.IsolatedContext && params.Fetch != nil {
		return nil, core.Error(core.ErrInvalidArgument, "isolatedContext cannot be used with fetchMode: true")
	}

	if params.Fetch != nil {
		return drv.openFetch(ctx, params)
	}

	page, err := drv.openPage(ctx, params, true)

	// No page is returned as a nil interface. A load that failed after the page was created
	// returns that page with the error, as before.
	if page == nil {
		return nil, err
	}

	return page, err
}

type (
	// connectFunc opens a tab and attaches to it, as createConnection does. The tab goes to
	// contextID when it is set, otherwise to the context of the driver.
	connectFunc func(ctx context.Context, params drivers.Params, contextID browser.ContextID, report *lazyAttachReport) (*rpcc.Conn, error)
	// isolateFunc creates the browser context of an isolatedContext call, as isolate does, and
	// returns its id with the function that disposes it.
	isolateFunc func(ctx context.Context, params drivers.Params) (browser.ContextID, func(), error)
	// loadFunc loads the page of an attached tab, as loadHTMLPage does.
	loadFunc func(ctx context.Context, conn *rpcc.Conn, params drivers.Params, report *lazyAttachReport) (*HTMLPage, error)
	// openFunc opens a new tab with its page, as openPage does.
	openFunc func(ctx context.Context, params drivers.Params, logConnErr bool) (*HTMLPage, error)
)

// openPage opens a new tab and loads a page in it, for Open and for a new fetchMode tab. A call
// with lazyAttach writes one info record to the ferret log, also when it fails.
func (drv *Driver) openPage(ctx context.Context, params drivers.Params, logConnErr bool) (*HTMLPage, error) {
	return drv.openPageWith(ctx, params, logConnErr, drv.isolate, drv.createConnection, loadHTMLPage)
}

// openPageWith is the one place that decides on the context of an isolatedContext call: a call
// that failed after the context was created disposes it at once, a page that loaded takes it
// along and disposes it when it is closed.
func (drv *Driver) openPageWith(
	ctx context.Context,
	params drivers.Params,
	logConnErr bool,
	isolate isolateFunc,
	connect connectFunc,
	load loadFunc,
) (page *HTMLPage, err error) {
	logger := logging.FromContext(ctx)

	var report *lazyAttachReport

	if params.LazyAttach != nil {
		report = newLazyAttachReport(params)
		// Known before the driver is initialized, so a call that fails there is still written
		// with the right flag.
		report.setIncognito(params.IsolatedContext || (!drv.options.KeepCookies && !params.KeepCookies))

		defer func() {
			report.log(logger, err)
		}()
	}

	var contextID browser.ContextID

	if params.IsolatedContext {
		var dispose func()

		contextID, dispose, err = isolate(ctx, params)

		if err != nil {
			report.fail(stagePrepare, err)

			if logConnErr {
				logger.Error().
					Err(err).
					Str("driver", drv.options.Name).
					Msg("failed to create a new connection")
			}

			return nil, err
		}

		// Deferred after the record above, so it runs first: total_ms of a failed call includes
		// the dispose.
		defer func() {
			if err == nil && page != nil {
				page.setDispose(dispose)

				return
			}

			dispose()
		}()
	}

	conn, err := connect(ctx, params, contextID, report)

	if err != nil {
		if logConnErr {
			logger.Error().
				Err(err).
				Str("driver", drv.options.Name).
				Msg("failed to create a new connection")
		}

		return nil, err
	}

	return load(ctx, conn, drv.setDefaultParams(params), report)
}

func (drv *Driver) Parse(ctx context.Context, params drivers.ParseParams) (drivers.HTMLPage, error) {
	logger := logging.FromContext(ctx)

	conn, err := drv.createConnection(ctx, drivers.Params{KeepCookies: true}, "", nil)

	if err != nil {
		logger.Error().
			Err(err).
			Str("driver", drv.options.Name).
			Msg("failed to create a new connection")

		return nil, err
	}

	return LoadHTMLPageWithContent(ctx, conn, drv.setDefaultParams(drivers.Params{
		URL:         BlankPageURL,
		UserAgent:   "",
		KeepCookies: params.KeepCookies,
		Cookies:     params.Cookies,
		Headers:     params.Headers,
		Viewport:    params.Viewport,
	}), params.Content)
}

func (drv *Driver) DoSimpleHTTPRequest(ctx context.Context, params drivers.Params) (*drivers.HTTPResponse, error) {
	return nil, core.ErrNotSupported
}

// openFetch hands out an open tab from the fetchMode pool of the key. A reused tab is
// returned as is: no navigation, so the query fetches from the page it left warmed up.
// A new tab is opened the ordinary way, on params.URL.
func (drv *Driver) openFetch(ctx context.Context, params drivers.Params) (drivers.HTMLPage, error) {
	return drv.openFetchWith(ctx, params, drv.openPage)
}

func (drv *Driver) openFetchWith(ctx context.Context, params drivers.Params, open openFunc) (drivers.HTMLPage, error) {
	logger := logging.FromContext(ctx)
	pool, key, err := drv.fetchPool(*params.Fetch, params.KeepCookies)

	if err != nil {
		return nil, err
	}

	// set by the open callback, which acquire runs in this goroutine
	opened := false

	tab, err := pool.acquire(ctx, func(ctx context.Context) (fetchTab, error) {
		opened = true
		// lazyAttach acts here only, on a tab the pool opens, and holds the place of that tab
		// until the page is loaded; a reused tab is handed out as is and writes no record.
		page, err := open(ctx, params, false)

		if err != nil {
			return nil, err
		}

		page.setRelease(func(page *HTMLPage) bool {
			return pool.release(page)
		})

		logger.Debug().
			Str("driver", drv.options.Name).
			Str("fetch_pool", key).
			Msg("opened a new fetchMode tab")

		return page, nil
	})

	if err != nil {
		logger.Error().
			Err(err).
			Str("driver", drv.options.Name).
			Str("fetch_pool", key).
			Msg("failed to get a fetchMode tab")

		return nil, err
	}

	page := tab.(*HTMLPage)

	// A reused tab keeps what it was opened with: logged, so a query that silently got
	// a tab meant for another URL can be spotted.
	if !opened {
		tabURL := ""

		if doc := page.getCurrentDocument(); doc != nil {
			tabURL = doc.GetURL().String()
		}

		logger.Debug().
			Str("driver", drv.options.Name).
			Str("fetch_pool", key).
			Str("url", params.URL).
			Str("tab_url", tabURL).
			Msg("reused a fetchMode tab")
	}

	return page, nil
}

// fetchPool returns the pool of the key. Size and limits are taken from the first
// DOCUMENT() that created the pool. Tabs opened with keepCookies live in the default
// browser context and incognito ones in the driver's context, so they never share a pool.
func (drv *Driver) fetchPool(params drivers.FetchParams, keepCookies bool) (*fetchPool, string, error) {
	key := params.PoolKey

	if key == "" {
		key = defaultFetchPoolKey
	}

	poolKey := key

	if keepCookies {
		poolKey += "\x00keepCookies"
	}

	drv.fetchMu.Lock()
	defer drv.fetchMu.Unlock()

	// after Close a new pool would restart the sweeper and open tabs nobody closes
	if drv.fetchClosed {
		return nil, key, errFetchPoolClosed
	}

	if drv.fetchPools == nil {
		drv.fetchPools = make(map[string]*fetchPool)
		drv.fetchStop = make(chan struct{})

		go drv.sweepFetchPools(drv.fetchStop)
	}

	pool, exists := drv.fetchPools[poolKey]

	if !exists {
		pool = newFetchPool(params)
		drv.fetchPools[poolKey] = pool
	}

	// marked while fetchMu is held, so the sweeper cannot drop the pool before acquire
	pool.touch()

	return pool, key, nil
}

// sweepFetchPools closes idle and abandoned fetchMode tabs and forgets pools nobody asks
// for, so rotating pool keys do not leave live tabs behind.
func (drv *Driver) sweepFetchPools(stop <-chan struct{}) {
	ticker := time.NewTicker(fetchSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		var gone []fetchTab

		drv.fetchMu.Lock()

		for key, pool := range drv.fetchPools {
			tabs, empty := pool.sweep()
			gone = append(gone, tabs...)

			if empty {
				delete(drv.fetchPools, key)
			}
		}

		drv.fetchMu.Unlock()

		destroyAll(gone)
	}
}

func (drv *Driver) closeFetchPools() {
	drv.fetchMu.Lock()
	pools := drv.fetchPools
	stop := drv.fetchStop
	drv.fetchPools = nil
	drv.fetchStop = nil
	drv.fetchClosed = true
	drv.fetchMu.Unlock()

	if stop != nil {
		close(stop)
	}

	for _, pool := range pools {
		pool.close()
	}
}

func (drv *Driver) Close() error {
	drv.closeFetchPools()

	drv.mu.Lock()
	defer drv.mu.Unlock()

	if drv.session != nil {
		drv.session.Close()

		return drv.conn.Close()
	}

	return nil
}

func (drv *Driver) createConnection(
	ctx context.Context,
	params drivers.Params,
	contextID browser.ContextID,
	report *lazyAttachReport,
) (*rpcc.Conn, error) {
	err := drv.init(ctx)

	if err != nil {
		report.fail(stagePrepare, err)

		return nil, errors.Wrap(err, "initialize driver")
	}

	return drv.opener(contextID).open(ctx, params, report)
}

// opener opens tabs in the context of the driver, or in contextID when it is set: the context of
// an isolatedContext call, which is incognito whatever the driver options are.
func (drv *Driver) opener(contextID browser.ContextID) targetOpener {
	o := targetOpener{
		targets:     drv.client.Target,
		cookies:     drv.client.Storage,
		dial:        drv.session.Dial,
		contextID:   drv.contextID,
		keepCookies: drv.options.KeepCookies,
	}

	if contextID != "" {
		o.contextID = contextID
		o.keepCookies = false
	}

	return o
}

// isolate creates the browser context of an isolatedContext call. Chrome disposes it together
// with its tabs if the connection of the driver drops.
func (drv *Driver) isolate(ctx context.Context, params drivers.Params) (browser.ContextID, func(), error) {
	if err := drv.init(ctx); err != nil {
		return "", nil, errors.Wrap(err, "initialize driver")
	}

	if err := ctx.Err(); err != nil {
		return "", nil, errors.Wrap(err, "create a browser context")
	}

	// The request may reach the browser even if ctx ends before the answer, and then the id
	// would be lost with the context left behind. So the context is created on a context of its
	// own and disposed at once if the call is over by then.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pageCloseTimeout)
	defer cancel()

	targets := drv.client.Target
	reply, err := targets.CreateBrowserContext(createCtx, (&target.CreateBrowserContextArgs{}).SetDisposeOnDetach(true))

	if err != nil {
		return "", nil, errors.Wrap(err, "create a browser context")
	}

	id := reply.BrowserContextID
	logger := logging.FromContext(ctx)
	dispose := func() {
		disposeContext(logger, targets, id, params.URL)
	}

	if err := ctx.Err(); err != nil {
		dispose()

		return "", nil, errors.Wrap(err, "create a browser context")
	}

	return id, dispose, nil
}

// contextDisposer is the part of the browser Target domain that disposes a browser context.
type contextDisposer interface {
	DisposeBrowserContext(context.Context, *target.DisposeBrowserContextArgs) error
}

// disposeContext disposes the browser context of an isolatedContext call on a context of its own,
// because the context of the call may be done by then. A failure is a warn: a context left behind
// holds browser processes and memory, and nothing else shows it. A closed browser connection is
// not, because Chrome disposes the context itself then (disposeOnDetach).
func disposeContext(logger zerolog.Logger, targets contextDisposer, id browser.ContextID, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), pageCloseTimeout)
	defer cancel()

	err := targets.DisposeBrowserContext(ctx, target.NewDisposeBrowserContextArgs(id))

	var closed interface{ Closed() bool }

	if err == nil || (errors.As(err, &closed) && closed.Closed()) {
		return
	}

	logger.Warn().
		Err(err).
		Str("browser_context_id", string(id)).
		Str("url", url).
		Msg("failed to dispose an isolated browser context")
}

type (
	// targetAPI is the part of the browser Target domain needed to open a tab.
	targetAPI interface {
		CreateTarget(context.Context, *target.CreateTargetArgs) (*target.CreateTargetReply, error)
		GetTargetInfo(context.Context, *target.GetTargetInfoArgs) (*target.GetTargetInfoReply, error)
		CloseTarget(context.Context, *target.CloseTargetArgs) (*target.CloseTargetReply, error)
	}

	cookieSetter interface {
		SetCookies(context.Context, *storage.SetCookiesArgs) error
	}

	// targetOpener creates a tab and attaches a DevTools session to it.
	targetOpener struct {
		targets     targetAPI
		cookies     cookieSetter
		dial        func(ctx context.Context, id target.ID) (*rpcc.Conn, error)
		contextID   browser.ContextID
		keepCookies bool
	}
)

func (o targetOpener) open(ctx context.Context, params drivers.Params, report *lazyAttachReport) (*rpcc.Conn, error) {
	// With DirectNavigation the tab opens straight on the target URL: no blank page and no
	// Page.navigate command afterwards.
	targetURL := BlankPageURL

	if params.DirectNavigation && params.URL != "" {
		targetURL = params.URL
	}

	// Args for a new target belonging to the browser context
	createTargetArgs := target.NewCreateTargetArgs(targetURL)

	incognito := !o.keepCookies && !params.KeepCookies

	if incognito {
		// Set it to an incognito mode
		createTargetArgs.SetBrowserContextID(o.contextID)
	}

	report.setIncognito(incognito)

	// The tab starts loading as soon as it is created, so cookies cannot be set on its
	// session anymore: they go to the browser context instead, before the target exists.
	if targetURL != BlankPageURL && params.Cookies != nil && params.Cookies.Length() > 0 {
		if err := o.setContextCookies(ctx, params, incognito); err != nil {
			report.fail(stagePrepare, err)

			return nil, errors.Wrap(err, "set cookies")
		}
	}

	// New target
	createTarget, err := o.targets.CreateTarget(ctx, createTargetArgs)

	if err != nil {
		report.fail(stagePrepare, err)

		return nil, errors.Wrap(err, "create a browser target")
	}

	report.setTarget(createTarget.TargetID)

	// With lazyAttach nothing is attached to the tab until the page comes to a verdict: the tab
	// is polled through the browser connection instead.
	if params.LazyAttach != nil && targetURL != BlankPageURL {
		verdict, err := awaitLazyVerdict(ctx, params.LazyAttach, params.URL, o.poll(createTarget.TargetID))

		report.setVerdict(verdict)

		if err != nil {
			o.closeTarget(createTarget.TargetID)
			report.fail(stagePoll, err)

			return nil, errors.Wrap(err, "await a lazyAttach verdict")
		}
	}

	// Connect to target using the existing websocket connection.
	conn, err := o.dial(ctx, createTarget.TargetID)

	if err != nil {
		// The tab exists but nothing is attached to it. ctx may be the very reason Dial
		// failed, so the tab is closed on a context of its own.
		o.closeTarget(createTarget.TargetID)
		report.fail(stageAttach, err)

		return nil, errors.Wrap(err, "establish a new connection")
	}

	return conn, nil
}

// poll reads the tab address, title and attached flag without attaching to the tab.
func (o targetOpener) poll(id target.ID) tabPoll {
	return func(ctx context.Context) (string, string, bool, error) {
		reply, err := o.targets.GetTargetInfo(ctx, target.NewGetTargetInfoArgs().SetTargetID(id))

		if err != nil {
			return "", "", false, err
		}

		info := reply.TargetInfo

		return info.URL, info.Title, info.Attached, nil
	}
}

// closeTarget closes a tab nothing is attached to. ctx of the call may already be done, so the
// tab is closed on a context of its own.
func (o targetOpener) closeTarget(id target.ID) {
	closeCtx, cancel := context.WithTimeout(context.Background(), pageCloseTimeout)
	_, _ = o.targets.CloseTarget(closeCtx, target.NewCloseTargetArgs(id))
	cancel()
}

func (o targetOpener) setContextCookies(ctx context.Context, params drivers.Params, incognito bool) error {
	cookies := make([]network.CookieParam, 0, params.Cookies.Length())

	params.Cookies.ForEach(func(value drivers.HTTPCookie, _ values.String) bool {
		cookies = append(cookies, net.FromDriverCookie(params.URL, value))

		return true
	})

	args := storage.NewSetCookiesArgs(cookies)

	if incognito {
		args.SetBrowserContextID(o.contextID)
	}

	return o.cookies.SetCookies(ctx, args)
}

func (drv *Driver) setDefaultParams(params drivers.Params) drivers.Params {
	if params.Viewport == nil {
		params.Viewport = defaultViewport
	}

	return drivers.SetDefaultParams(drv.options.Options, params)
}

func (drv *Driver) init(ctx context.Context) error {
	drv.mu.Lock()
	defer drv.mu.Unlock()

	if drv.session == nil {
		ver, err := drv.dev.Version(ctx)

		if err != nil {
			return errors.Wrap(err, "failed to initialize driver")
		}

		dialOpts := make([]rpcc.DialOption, 0, 2)

		if drv.options.Connection != nil {
			if drv.options.Connection.BufferSize > 0 {
				dialOpts = append(dialOpts, rpcc.WithWriteBufferSize(drv.options.Connection.BufferSize))
			}

			if drv.options.Connection.Compression {
				dialOpts = append(dialOpts, rpcc.WithCompression())
			}
		}

		bconn, err := rpcc.DialContext(
			ctx,
			ver.WebSocketDebuggerURL,
			dialOpts...,
		)

		if err != nil {
			return errors.Wrap(err, "failed to initialize driver")
		}

		bc := cdp.NewClient(bconn)

		sess, err := session.NewManager(bc)

		if err != nil {
			bconn.Close()

			return errors.Wrap(err, "failed to initialize driver")
		}

		drv.conn = bconn
		drv.client = bc
		drv.session = sess

		if drv.options.KeepCookies {
			return nil
		}

		// Chrome disposes the context with all its tabs when this connection drops, e.g. the
		// process exits without Close. Otherwise tabs stay open in a shared browser after
		// every restart — fetchMode tabs keep running page scripts until Chrome restarts.
		createCtx, err := bc.Target.CreateBrowserContext(ctx, (&target.CreateBrowserContextArgs{}).SetDisposeOnDetach(true))

		if err != nil {
			bconn.Close()
			sess.Close()

			return err
		}

		drv.contextID = createCtx.BrowserContextID
	}

	return nil
}
