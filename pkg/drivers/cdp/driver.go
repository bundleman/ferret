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
	if params.Fetch != nil {
		return drv.openFetch(ctx, params)
	}

	logger := logging.FromContext(ctx)
	conn, err := drv.createConnection(ctx, params)

	if err != nil {
		logger.Error().
			Err(err).
			Str("driver", drv.options.Name).
			Msg("failed to create a new connection")

		return nil, err
	}

	return LoadHTMLPage(ctx, conn, drv.setDefaultParams(params))
}

func (drv *Driver) Parse(ctx context.Context, params drivers.ParseParams) (drivers.HTMLPage, error) {
	logger := logging.FromContext(ctx)

	conn, err := drv.createConnection(ctx, drivers.Params{KeepCookies: true})

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
	logger := logging.FromContext(ctx)
	pool, key, err := drv.fetchPool(*params.Fetch, params.KeepCookies)

	if err != nil {
		return nil, err
	}

	// set by the open callback, which acquire runs in this goroutine
	opened := false

	tab, err := pool.acquire(ctx, func(ctx context.Context) (fetchTab, error) {
		opened = true
		conn, err := drv.createConnection(ctx, params)

		if err != nil {
			return nil, err
		}

		page, err := LoadHTMLPage(ctx, conn, drv.setDefaultParams(params))

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

func (drv *Driver) createConnection(ctx context.Context, params drivers.Params) (*rpcc.Conn, error) {
	err := drv.init(ctx)

	if err != nil {
		return nil, errors.Wrap(err, "initialize driver")
	}

	// With DirectNavigation the tab opens straight on the target URL: no blank page and no
	// Page.navigate command afterwards.
	targetURL := BlankPageURL

	if params.DirectNavigation && params.URL != "" {
		targetURL = params.URL
	}

	// Args for a new target belonging to the browser context
	createTargetArgs := target.NewCreateTargetArgs(targetURL)

	incognito := !drv.options.KeepCookies && !params.KeepCookies

	if incognito {
		// Set it to an incognito mode
		createTargetArgs.SetBrowserContextID(drv.contextID)
	}

	// The tab starts loading as soon as it is created, so cookies cannot be set on its
	// session anymore: they go to the browser context instead, before the target exists.
	if targetURL != BlankPageURL && params.Cookies != nil && params.Cookies.Length() > 0 {
		if err := drv.setContextCookies(ctx, params, incognito); err != nil {
			return nil, errors.Wrap(err, "set cookies")
		}
	}

	// New target
	createTarget, err := drv.client.Target.CreateTarget(ctx, createTargetArgs)

	if err != nil {
		return nil, errors.Wrap(err, "create a browser target")
	}

	// Connect to target using the existing websocket connection.
	conn, err := drv.session.Dial(ctx, createTarget.TargetID)

	if err != nil {
		// The tab exists but nothing is attached to it. ctx may be the very reason Dial
		// failed, so the tab is closed on a context of its own.
		closeCtx, cancel := context.WithTimeout(context.Background(), pageCloseTimeout)
		_, _ = drv.client.Target.CloseTarget(closeCtx, target.NewCloseTargetArgs(createTarget.TargetID))
		cancel()

		return nil, errors.Wrap(err, "establish a new connection")
	}

	return conn, nil
}

func (drv *Driver) setContextCookies(ctx context.Context, params drivers.Params, incognito bool) error {
	cookies := make([]network.CookieParam, 0, params.Cookies.Length())

	params.Cookies.ForEach(func(value drivers.HTTPCookie, _ values.String) bool {
		cookies = append(cookies, net.FromDriverCookie(params.URL, value))

		return true
	})

	args := storage.NewSetCookiesArgs(cookies)

	if incognito {
		args.SetBrowserContextID(drv.contextID)
	}

	return drv.client.Storage.SetCookies(ctx, args)
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
