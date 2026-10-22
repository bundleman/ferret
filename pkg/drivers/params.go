package drivers

import (
	"io"
	"time"
)

type (
	ResourceFilter struct {
		URL  string
		Type string
	}

	StatusCodeFilter struct {
		URL  string
		Code int
	}

	Ignore struct {
		Resources   []ResourceFilter
		StatusCodes []StatusCodeFilter
	}

	Viewport struct {
		Height      int
		Width       int
		ScaleFactor float64
		Mobile      bool
		Landscape   bool
	}

	Params struct {
		URL               string
		UserAgent         string
		KeepCookies       bool
		Cookies           *HTTPCookies
		Headers           *HTTPHeaders
		Viewport          *Viewport
		Charset           string
		Ignore            *Ignore
		SimpleHTTPRequest *SimpleHTTPRequest
		MaxRedirectsLimit uint8
		EvaluateArgs      *EvaluateArgs
		DisableRuntime    bool
		DirectNavigation  bool
		Fetch             *FetchParams
	}

	// FetchParams keeps a CDP tab open between queries: the page is not closed when the
	// query ends but returned to a pool, and the next DOCUMENT() with the same key gets it
	// as is, without navigation. Used to fetch data from inside an already warmed page.
	FetchParams struct {
		// PoolKey groups tabs that can replace each other, e.g. a proxy or a region.
		PoolKey string
		// PoolSize is the maximum number of open tabs per key.
		PoolSize int
		// MaxUses is how many queries a tab serves before it is closed and reopened.
		MaxUses int
		// MaxAge is how long a tab lives before it is closed and reopened.
		MaxAge time.Duration
	}

	SimpleHTTPRequest struct {
		Method string
		Body   io.Reader
	}

	ParseParams struct {
		Content     []byte
		KeepCookies bool
		Cookies     *HTTPCookies
		Headers     *HTTPHeaders
		Viewport    *Viewport
	}

	EvaluateArgs struct {
		Expression string
		// OnEveryNewDocument installs the script via
		// Page.addScriptToEvaluateOnNewDocument so it runs on every new
		// document (initial load, back/forward, reload, client-side redirect)
		// before page scripts. Default false keeps the legacy behaviour:
		// a single Runtime.Evaluate after Navigate().
		OnEveryNewDocument bool
	}
)
