package html

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
	"github.com/MontFerret/ferret/pkg/runtime/values/types"
)

type PageLoadParams struct {
	drivers.Params
	Driver  string
	Timeout time.Duration
}

// DOCUMENT opens an HTML page by a given url.
// By default, loads a page by http call - resulted page does not support any interactions.
// @param {Object} [params] - An object containing the following properties :
// @param {String} [params.driver] - Driver name to use.
// @param {Int} [params.timeout=60000] - Page load timeout.
// @param {String} [params.userAgent] - Custom user agent.
// @param {Boolean} [params.keepCookies=False] - Boolean value indicating whether to use cookies from previous sessions i.e. not to open a page in the Incognito mode.
// @param {Object[] | Object} [params.cookies] - Set of HTTP cookies to use during page loading.
// @param {String} params.cookies.*.name - Cookie name.
// @param {String} params.cookies.*.value - Cookie value.
// @param {String} params.cookies.*.path - Cookie path.
// @param {String} params.cookies.*.domain - Cookie domain.
// @param {Int} [params.cookies.*.maxAge] - Cookie max age.
// @param {String|DateTime} [params.cookies.*.expires] - Cookie expiration date time.
// @param {String} [params.cookies.*.sameSite] - Cookie cross-origin policy.
// @param {Boolean} [params.cookies.*.httpOnly=false] - Cookie cannot be accessed through client side script.
// @param {Boolean} [params.cookies.*.secure=false] - Cookie sent to the server only with an encrypted request over the HTTPS protocol.
// @param {Object} [params.headers] - Set of HTTP headers to use during page loading.
// @param {Object} [params.ignore] - Set of parameters to ignore some page functionality or behavior.
// @param {Object[]} [params.ignore.resources] - Collection of rules to ignore resources during page load and navigation.
// @param {String} [params.ignore.resources.*.url] - Resource url pattern. If set, requests for matching urls will be blocked. Wildcards ('*' -> zero or more, '?' -> exactly one) are allowed. Escape character is backslash. Omitting is equivalent to "*".
// @param {String} [params.ignore.resources.*.type] - Resource type. If set, requests for matching resource types will be blocked.
// @param {Object[]} [params.ignore.statusCodes] - Collection of rules to ignore certain HTTP codes that can cause failures.
// @param {String} [params.ignore.statusCodes.*.url] - Url pattern. If set, codes for matching urls will be ignored. Wildcards ('*' -> zero or more, '?' -> exactly one) are allowed. Escape character is backslash. Omitting is equivalent to "*".
// @param {Int} [params.ignore.statusCodes.*.code] - HTTP code to ignore.
// @param {Object} [params.viewport] - Viewport params.
// @param {Int} [params.viewport.height] - Viewport height.
// @param {Int} [params.viewport.width] - Viewport width.
// @param {Float} [params.viewport.scaleFactor] - Viewport scale factor.
// @param {Boolean} [params.viewport.mobile] - Value that indicates whether to emulate mobile device.
// @param {Boolean} [params.viewport.landscape] - Value that indicates whether to render a page in landscape position.
// @param {String} [params.charset] - (only HTTPDriver) Source charset content to convert UTF-8.
// @param {Object} [params.evaluateArgs] - Set of parameters to add evaluate arguments.
// @param {String} [params.evaluateArgs.expression] - JS script for execution.
// @param {Boolean} [params.evaluateArgs.onEveryNewDocument=false] - (only CDPDriver) When true, the script is registered via Page.addScriptToEvaluateOnNewDocument and runs on every new document (initial load, back/forward, reload, client-side redirect) before any page scripts. When false, the script is executed once via Runtime.Evaluate after Navigate().
// @param {Boolean} [params.disableRuntime=false] - (only CDPDriver) Skip Runtime.enable for the page, so V8 does not switch into debug mode.
// @param {Boolean | Object} [params.directNavigation=false] - (only CDPDriver) Create the tab straight on the target URL instead of opening about:blank and navigating it. An object {lazyAttach: {...}} means the same with a lazy attach.
// @param {Object} [params.directNavigation.lazyAttach] - (checked for any driver, used only by CDPDriver) Do not attach to the new tab until the page comes to a verdict: the tab is polled through the browser connection, and a DevTools session appears on it only after the verdict or after lazyAttach.timeout. Then the driver waits only for the end of HTML parsing (document.readyState is not "loading"), not for the load event: waiting for the page's own data is up to the query. Unlike directNavigation: true, any error of that wait fails DOCUMENT() and closes the tab. userAgent, headers, viewport, ignore and evaluateArgs apply from the attach on; with evaluateArgs.onEveryNewDocument the script runs only in documents committed after the attach, so usually not in the document DOCUMENT() returns. Cookies are set before the tab is created, as with directNavigation. The response of the main document is usually not seen, so use doc?.response. Polls with an empty address are skipped; url is checked before title, so a page matching both is a verdict by url. The document URL must not be empty or about:blank. A parse error of the object is also written to the log at warn level with the URL.
// @param {Int} [params.directNavigation.lazyAttach.timeout=20000] - How long, in milliseconds, to wait for a verdict without attaching. Counted from the first poll; must be less than params.timeout. The end of it is not an error: the driver attaches as usual.
// @param {Int} [params.directNavigation.lazyAttach.interval=300] - How often, in milliseconds, to poll the tab, counted from the end of the previous poll. Must be greater than zero and not greater than timeout.
// @param {String[]} [params.directNavigation.lazyAttach.url] - Patterns of the tab address; a match is a verdict. The whole raw address is matched, case-sensitive and without decoding %XX: '*' is any run of characters, '?' is exactly one character, '\' makes the next character ordinary, every other character is ordinary, and a pattern ending with an unpaired '\' is an error. A pattern must not match the requested URL.
// @param {String[]} [params.directNavigation.lazyAttach.title] - Patterns of the tab title, same syntax; a match is a verdict. Chrome's placeholder (the tab address shown while the document has no title of its own) never matches, so ["*"] means "the page has a title". url or title must be set, and none of them may be empty or hold an empty pattern.
// @param {Boolean} [params.directNavigation.lazyAttach.waitReadyOnURL=true] - Wait for the end of HTML parsing after a verdict by url. false skips that wait, for url patterns that tell the outcome without the page content (e.g. a captcha address); a document not committed yet is still waited for until its first navigation event. Only allowed together with url.
// @param {Boolean} [params.fetchMode=false] - (only CDPDriver) Keep the tab open after the query: it goes back to a pool, and the next DOCUMENT() with the same fetchPoolKey gets this tab as is, without navigation. For fetching data from inside an already warmed page. Assign the document to a variable (LET), otherwise the tab is not returned to the pool. Other params apply only when a new tab is opened. A query must be done with the tab within 10 minutes: after that the tab is taken back and closed even if the query still uses it.
// @param {String} [params.fetchPoolKey="default"] - (only CDPDriver) Pool key for fetchMode tabs, e.g. a proxy or a region.
// @param {Int} [params.fetchPoolSize=1] - (only CDPDriver) Maximum open tabs per fetchPoolKey. A query waits for a free tab up to params.timeout.
// @param {Int} [params.fetchMaxUses=500] - (only CDPDriver) Queries served by a tab before it is closed and reopened.
// @param {Int} [params.fetchMaxAge=1800] - (only CDPDriver) Tab lifetime in seconds before it is closed and reopened. Unlike params.timeout, which is in milliseconds.
// @return {HTMLPage} - Loaded HTML page.
func Open(ctx context.Context, args ...core.Value) (core.Value, error) {
	err := core.ValidateArgs(args, 1, 2)

	if err != nil {
		return values.None, err
	}

	err = core.ValidateType(args[0], types.String)

	if err != nil {
		return values.None, err
	}

	url := args[0].(values.String)

	var params PageLoadParams

	if len(args) == 1 {
		params = newDefaultDocLoadParams(url)
	} else {
		p, err := newPageLoadParams(url, args[1])

		if err != nil {
			// A query may swallow the error of DOCUMENT() with '?', so a broken lazyAttach would
			// otherwise go unnoticed.
			if errors.Is(err, errDirectNavigation) {
				logger := logging.FromContext(ctx)
				logger.Warn().
					Err(err).
					Str("url", url.String()).
					Msg("failed to parse directNavigation of DOCUMENT()")
			}

			return values.None, err
		}

		params = p
	}

	ctx, cancel := context.WithTimeout(ctx, params.Timeout)
	defer cancel()

	drv, err := drivers.FromContext(ctx, params.Driver)
	if err != nil {
		return values.None, err
	}

	if params.SimpleHTTPRequest != nil {
		return drv.DoSimpleHTTPRequest(ctx, params.Params)

	}

	return drv.Open(ctx, params.Params)
}

func newDefaultDocLoadParams(url values.String) PageLoadParams {
	return PageLoadParams{
		Params: drivers.Params{
			URL: url.String(),
		},
		Timeout: drivers.DefaultPageLoadTimeout * time.Millisecond,
	}
}

func newPageLoadParams(url values.String, arg core.Value) (PageLoadParams, error) {
	res := newDefaultDocLoadParams(url)

	if err := core.ValidateType(arg, types.Boolean, types.String, types.Object); err != nil {
		return res, err
	}

	switch arg.Type() {
	case types.Object:
		obj := arg.(*values.Object)

		driver, exists := obj.Get(values.NewString("driver"))

		if exists {
			if err := core.ValidateType(driver, types.String); err != nil {
				return res, err
			}

			res.Driver = driver.(values.String).String()
		}

		timeout, exists := obj.Get(values.NewString("timeout"))

		if exists {
			if err := core.ValidateType(timeout, types.Int); err != nil {
				return res, err
			}

			res.Timeout = time.Duration(timeout.(values.Int)) * time.Millisecond
		}

		userAgent, exists := obj.Get(values.NewString("userAgent"))

		if exists {
			if err := core.ValidateType(userAgent, types.String); err != nil {
				return res, err
			}

			res.UserAgent = userAgent.String()
		}

		keepCookies, exists := obj.Get(values.NewString("keepCookies"))

		if exists {
			if err := core.ValidateType(keepCookies, types.Boolean); err != nil {
				return res, err
			}

			res.KeepCookies = bool(keepCookies.(values.Boolean))
		}

		disableRuntime, exists := obj.Get(values.NewString("disableRuntime"))

		if exists {
			if err := core.ValidateType(disableRuntime, types.Boolean); err != nil {
				return res, err
			}

			res.DisableRuntime = bool(disableRuntime.(values.Boolean))
		}

		directNavigation, exists := obj.Get(values.NewString("directNavigation"))

		if exists {
			direct, lazy, err := parseDirectNavigation(directNavigation, res.URL, res.Timeout)

			if err != nil {
				return res, err
			}

			res.DirectNavigation = direct
			res.LazyAttach = lazy
		}

		fetch, err := parseFetchParams(obj)

		if err != nil {
			return res, err
		}

		res.Fetch = fetch

		cookies, exists := obj.Get(values.NewString("cookies"))

		if exists {
			if err := core.ValidateType(cookies, types.Array, types.Object); err != nil {
				return res, err
			}

			switch c := cookies.(type) {
			case *values.Array:
				cookies, err := parseCookieArray(c)

				if err != nil {
					return res, err
				}

				res.Cookies = cookies
			case *values.Object:
				cookies, err := parseCookieObject(c)

				if err != nil {
					return res, err
				}

				res.Cookies = cookies
			default:
				res.Cookies = drivers.NewHTTPCookies()
			}
		}

		headers, exists := obj.Get(values.NewString("headers"))

		if exists {
			if err := core.ValidateType(headers, types.Object); err != nil {
				return res, err
			}

			header := parseHeader(headers.(*values.Object))
			res.Headers = header
		}

		viewport, exists := obj.Get(values.NewString("viewport"))

		if exists {
			viewport, err := parseViewport(viewport)

			if err != nil {
				return res, err
			}

			res.Viewport = viewport
		}

		ignore, exists := obj.Get(values.NewString("ignore"))

		if exists {
			ignore, err := parseIgnore(ignore)

			if err != nil {
				return res, err
			}

			res.Ignore = ignore
		}

		charset, exists := obj.Get(values.NewString("charset"))
		if exists {
			if err := core.ValidateType(charset, types.String); err != nil {
				return res, err
			}

			res.Charset = charset.String()
		}

		simpleRequest, exists := obj.Get(values.NewString("simpleRequest"))
		if exists {
			simpleRequest, err := parseSimpleHTTPRequest(simpleRequest)

			if err != nil {
				return res, err
			}

			res.SimpleHTTPRequest = simpleRequest
		}

		maxRedirectsLimit, exists := obj.Get("maxRedirectsLimit")
		if exists {
			res.MaxRedirectsLimit = uint8(values.ToInt(maxRedirectsLimit))
		}

		evaluateArgsRaw, exists := obj.Get(values.NewString("evaluateArgs"))
		if exists {
			evaluateArgs, err := parseSimpleEvaluateArgs(evaluateArgsRaw)
			if err != nil {
				return res, err
			}

			res.EvaluateArgs = evaluateArgs
		}

	case types.String:
		res.Driver = arg.(values.String).String()
	case types.Boolean:
		b := arg.(values.Boolean)

		// fallback
		if b {
			res.Driver = cdp.DriverName
		}
	}

	return res, nil
}

func parseCookieObject(obj *values.Object) (*drivers.HTTPCookies, error) {
	if obj == nil {
		return nil, errors.Wrap(core.ErrMissedArgument, "cookies")
	}

	var err error
	res := drivers.NewHTTPCookies()

	obj.ForEach(func(value core.Value, _ string) bool {
		cookie, e := parseCookie(value)

		if e != nil {
			err = e

			return false
		}

		res.Set(cookie)

		return true
	})

	return res, err
}

func parseCookieArray(arr *values.Array) (*drivers.HTTPCookies, error) {
	if arr == nil {
		return nil, errors.Wrap(core.ErrMissedArgument, "cookies")
	}

	var err error
	res := drivers.NewHTTPCookies()

	arr.ForEach(func(value core.Value, _ int) bool {
		cookie, e := parseCookie(value)

		if e != nil {
			err = e

			return false
		}

		res.Set(cookie)

		return true
	})

	return res, err
}

func parseCookie(value core.Value) (drivers.HTTPCookie, error) {
	err := core.ValidateType(value, types.Object, drivers.HTTPCookieType)

	if err != nil {
		return drivers.HTTPCookie{}, err
	}

	if value.Type() == drivers.HTTPCookieType {
		return value.(drivers.HTTPCookie), nil
	}

	co := value.(*values.Object)

	cookie := drivers.HTTPCookie{
		Name:   co.MustGet("name").String(),
		Value:  co.MustGet("value").String(),
		Path:   co.MustGet("path").String(),
		Domain: co.MustGet("domain").String(),
	}

	maxAge, exists := co.Get("maxAge")

	if exists {
		if err = core.ValidateType(maxAge, types.Int); err != nil {
			return drivers.HTTPCookie{}, err
		}

		cookie.MaxAge = int(maxAge.(values.Int))
	}

	expires, exists := co.Get("expires")

	if exists {
		if err = core.ValidateType(expires, types.DateTime, types.String); err != nil {
			return drivers.HTTPCookie{}, err
		}

		if expires.Type() == types.DateTime {
			cookie.Expires = expires.(values.DateTime).Unwrap().(time.Time)
		} else {
			t, err := time.Parse(values.DefaultTimeLayout, expires.String())

			if err != nil {
				return drivers.HTTPCookie{}, err
			}

			cookie.Expires = t
		}
	}

	sameSite, exists := co.Get("sameSite")

	if exists {
		sameSite := strings.ToLower(sameSite.String())

		switch sameSite {
		case "lax":
			cookie.SameSite = drivers.SameSiteLaxMode
		case "strict":
			cookie.SameSite = drivers.SameSiteStrictMode
		default:
			cookie.SameSite = drivers.SameSiteDefaultMode
		}
	}

	httpOnly, exists := co.Get("httpOnly")

	if exists {
		if err = core.ValidateType(httpOnly, types.Boolean); err != nil {
			return drivers.HTTPCookie{}, err
		}

		cookie.HTTPOnly = bool(httpOnly.(values.Boolean))
	}

	secure, exists := co.Get("secure")

	if exists {
		if err = core.ValidateType(secure, types.Boolean); err != nil {
			return drivers.HTTPCookie{}, err
		}

		cookie.Secure = bool(secure.(values.Boolean))
	}

	return cookie, err
}

func parseHeader(headers *values.Object) *drivers.HTTPHeaders {
	res := drivers.NewHTTPHeaders()

	headers.ForEach(func(value core.Value, key string) bool {
		if value.Type() == types.Array {
			value := value.(*values.Array)

			keyValues := make([]string, 0, value.Length())

			value.ForEach(func(v core.Value, idx int) bool {
				keyValues = append(keyValues, v.String())

				return true
			})

			res.SetArr(key, keyValues)
		} else {
			res.Set(key, value.String())
		}

		return true
	})

	return res
}

func parseViewport(value core.Value) (*drivers.Viewport, error) {
	if err := core.ValidateType(value, types.Object); err != nil {
		return nil, err
	}

	res := &drivers.Viewport{}

	viewport := value.(*values.Object)

	width, exists := viewport.Get(values.NewString("width"))

	if exists {
		if err := core.ValidateType(width, types.Int); err != nil {
			return nil, err
		}

		res.Width = int(values.ToInt(width))
	}

	height, exists := viewport.Get(values.NewString("height"))

	if exists {
		if err := core.ValidateType(height, types.Int); err != nil {
			return nil, err
		}

		res.Height = int(values.ToInt(height))
	}

	mobile, exists := viewport.Get(values.NewString("mobile"))

	if exists {
		res.Mobile = bool(values.ToBoolean(mobile))
	}

	landscape, exists := viewport.Get(values.NewString("landscape"))

	if exists {
		res.Landscape = bool(values.ToBoolean(landscape))
	}

	scaleFactor, exists := viewport.Get(values.NewString("scaleFactor"))

	if exists {
		res.ScaleFactor = float64(values.ToFloat(scaleFactor))
	}

	return res, nil
}

func parseIgnore(value core.Value) (*drivers.Ignore, error) {
	if err := core.ValidateType(value, types.Object); err != nil {
		return nil, err
	}

	res := &drivers.Ignore{}

	ignore := value.(*values.Object)

	resources, exists := ignore.Get("resources")

	if exists {
		if err := core.ValidateType(resources, types.Array); err != nil {
			return nil, err
		}

		resources := resources.(*values.Array)

		res.Resources = make([]drivers.ResourceFilter, 0, resources.Length())

		var e error

		resources.ForEach(func(el core.Value, idx int) bool {
			if e = core.ValidateType(el, types.Object); e != nil {
				return false
			}

			pattern := el.(*values.Object)

			url, urlExists := pattern.Get("url")
			resType, resTypeExists := pattern.Get("type")

			// ignore element
			if !urlExists && !resTypeExists {
				return true
			}

			res.Resources = append(res.Resources, drivers.ResourceFilter{
				URL:  url.String(),
				Type: resType.String(),
			})

			return true
		})

		if e != nil {
			return nil, e
		}
	}

	statusCodes, exists := ignore.Get("statusCodes")

	if exists {
		if err := core.ValidateType(statusCodes, types.Array); err != nil {
			return nil, err
		}

		statusCodes := statusCodes.(*values.Array)

		res.StatusCodes = make([]drivers.StatusCodeFilter, 0, statusCodes.Length())

		var e error

		statusCodes.ForEach(func(el core.Value, idx int) bool {
			if e = core.ValidateType(el, types.Object); e != nil {
				return false
			}

			pattern := el.(*values.Object)

			url := pattern.MustGetOr("url", values.NewString(""))
			code, codeExists := pattern.Get("code")

			// ignore element
			if !codeExists {
				e = errors.New("http code is required")
				return false
			}

			res.StatusCodes = append(res.StatusCodes, drivers.StatusCodeFilter{
				URL:  url.String(),
				Code: int(values.ToInt(code)),
			})

			return true
		})
	}

	return res, nil
}

func parseSimpleHTTPRequest(value core.Value) (*drivers.SimpleHTTPRequest, error) {
	if err := core.ValidateType(value, types.Object); err != nil {
		return nil, err
	}

	req := &drivers.SimpleHTTPRequest{}

	reqObj := value.(*values.Object)

	method, methodExists := reqObj.Get("method")
	body, bodyExists := reqObj.Get("body")

	if !methodExists {
		return nil, errors.New("method doesn't exists")
	}

	if bodyExists {
		if err := core.ValidateType(body, types.Binary); err != nil {
			return nil, core.Error(err, ".body")
		}

		req.Body = bytes.NewBuffer(body.(values.Binary))
	}

	req.Method = method.String()

	return req, nil
}

func parseSimpleEvaluateArgs(value core.Value) (*drivers.EvaluateArgs, error) {
	if err := core.ValidateType(value, types.Object); err != nil {
		return nil, err
	}

	args := &drivers.EvaluateArgs{}

	reqObj := value.(*values.Object)

	expression, expressionExists := reqObj.Get("expression")
	if !expressionExists {
		return nil, errors.New("expression doesn't exists")
	}

	args.Expression = expression.String()

	if v, ok := reqObj.Get("onEveryNewDocument"); ok {
		args.OnEveryNewDocument = bool(values.ToBoolean(v))
	}

	return args, nil
}

// parseFetchParams reads fetchMode and its pool settings. Returns nil when fetchMode is off,
// defaults for the omitted fields are filled by the driver.
func parseFetchParams(obj *values.Object) (*drivers.FetchParams, error) {
	mode, exists := obj.Get(values.NewString("fetchMode"))

	if !exists {
		return nil, nil
	}

	if err := core.ValidateType(mode, types.Boolean); err != nil {
		return nil, err
	}

	if !bool(mode.(values.Boolean)) {
		return nil, nil
	}

	res := &drivers.FetchParams{}

	key, exists := obj.Get(values.NewString("fetchPoolKey"))

	if exists {
		if err := core.ValidateType(key, types.String); err != nil {
			return nil, err
		}

		res.PoolKey = key.String()
	}

	size, exists := obj.Get(values.NewString("fetchPoolSize"))

	if exists {
		if err := core.ValidateType(size, types.Int); err != nil {
			return nil, err
		}

		res.PoolSize = int(size.(values.Int))
	}

	maxUses, exists := obj.Get(values.NewString("fetchMaxUses"))

	if exists {
		if err := core.ValidateType(maxUses, types.Int); err != nil {
			return nil, err
		}

		res.MaxUses = int(maxUses.(values.Int))
	}

	maxAge, exists := obj.Get(values.NewString("fetchMaxAge"))

	if exists {
		if err := core.ValidateType(maxAge, types.Int); err != nil {
			return nil, err
		}

		res.MaxAge = time.Duration(maxAge.(values.Int)) * time.Second
	}

	return res, nil
}

// errDirectNavigation marks an error of parsing the object form of directNavigation, so Open can
// log it.
var errDirectNavigation = errors.New("directNavigation")

func directNavigationErrorf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", errDirectNavigation, fmt.Sprintf(format, args...))
}

// parseDirectNavigation reads directNavigation: a boolean as before, or an object
// {lazyAttach: {...}} meaning direct navigation with a lazy attach. Defaults are put in place
// before the checks, and the checks do not depend on the driver that will run the call.
func parseDirectNavigation(value core.Value, docURL string, callTimeout time.Duration) (bool, *drivers.LazyAttach, error) {
	if b, ok := value.(values.Boolean); ok {
		return bool(b), nil, nil
	}

	obj, ok := value.(*values.Object)

	if !ok {
		// Still an invalid type error, as before the object form existed. It is not a lazyAttach
		// error, so it is not marked for the warn record.
		return false, nil, core.TypeError(value.Type(), types.Boolean, types.Object)
	}

	raw, exists := obj.Get(values.NewString("lazyAttach"))

	if !exists || obj.Length() != 1 {
		return false, nil, directNavigationErrorf("an object must hold lazyAttach and nothing else")
	}

	lazyObj, ok := raw.(*values.Object)

	if !ok {
		return false, nil, directNavigationErrorf("lazyAttach must be an object, got %s", raw.Type())
	}

	lazy, err := parseLazyAttach(lazyObj)

	if err != nil {
		return false, nil, err
	}

	if lazy.Timeout >= callTimeout {
		return false, nil, directNavigationErrorf(
			"lazyAttach.timeout %d ms must be less than the DOCUMENT() timeout %d ms",
			lazy.Timeout.Milliseconds(), callTimeout.Milliseconds(),
		)
	}

	if docURL == "" || docURL == cdp.BlankPageURL {
		return false, nil, directNavigationErrorf("lazyAttach needs a document URL, got %q", docURL)
	}

	// A pattern matching the requested address would give the verdict on the first poll.
	for _, pattern := range lazy.URL {
		re, _ := drivers.CompileLazyPattern(pattern)

		if re.MatchString(docURL) {
			return false, nil, directNavigationErrorf("lazyAttach.url pattern %q matches the requested URL %q", pattern, docURL)
		}
	}

	return true, lazy, nil
}

func parseLazyAttach(obj *values.Object) (*drivers.LazyAttach, error) {
	res := &drivers.LazyAttach{
		Timeout:        drivers.DefaultLazyAttachTimeout * time.Millisecond,
		Interval:       drivers.DefaultLazyAttachInterval * time.Millisecond,
		WaitReadyOnURL: true,
	}

	var unknown string

	obj.ForEach(func(_ core.Value, key string) bool {
		switch key {
		case "timeout", "interval", "url", "title", "waitReadyOnURL":
			return true
		default:
			unknown = key

			return false
		}
	})

	if unknown != "" {
		return nil, directNavigationErrorf("unknown lazyAttach key %q", unknown)
	}

	for _, field := range []struct {
		name string
		dst  *time.Duration
	}{{"timeout", &res.Timeout}, {"interval", &res.Interval}} {
		value, exists := obj.Get(values.NewString(field.name))

		if !exists {
			continue
		}

		if err := core.ValidateType(value, types.Int); err != nil {
			return nil, directNavigationErrorf("lazyAttach.%s: %s", field.name, err)
		}

		*field.dst = time.Duration(value.(values.Int)) * time.Millisecond
	}

	var err error

	if res.URL, err = parseLazyPatterns(obj, "url"); err != nil {
		return nil, err
	}

	if res.Title, err = parseLazyPatterns(obj, "title"); err != nil {
		return nil, err
	}

	if value, exists := obj.Get(values.NewString("waitReadyOnURL")); exists {
		if err := core.ValidateType(value, types.Boolean); err != nil {
			return nil, directNavigationErrorf("lazyAttach.waitReadyOnURL: %s", err)
		}

		// Without url patterns there is no verdict by URL for the flag to act on.
		if res.URL == nil {
			return nil, directNavigationErrorf("lazyAttach.waitReadyOnURL is set without lazyAttach.url")
		}

		res.WaitReadyOnURL = bool(value.(values.Boolean))
	}

	if res.URL == nil && res.Title == nil {
		return nil, directNavigationErrorf("lazyAttach needs url or title patterns")
	}

	if res.Timeout <= 0 || res.Interval <= 0 {
		return nil, directNavigationErrorf(
			"lazyAttach.timeout %d ms and lazyAttach.interval %d ms must be greater than zero",
			res.Timeout.Milliseconds(), res.Interval.Milliseconds(),
		)
	}

	if res.Interval > res.Timeout {
		return nil, directNavigationErrorf(
			"lazyAttach.interval %d ms must not be greater than lazyAttach.timeout %d ms",
			res.Interval.Milliseconds(), res.Timeout.Milliseconds(),
		)
	}

	return res, nil
}

// parseLazyPatterns reads an array of patterns. An empty array or an empty pattern would never
// give a verdict: polls with an empty address are skipped and an empty title is a placeholder.
func parseLazyPatterns(obj *values.Object, key string) ([]string, error) {
	value, exists := obj.Get(values.NewString(key))

	if !exists {
		return nil, nil
	}

	arr, ok := value.(*values.Array)

	if !ok {
		return nil, directNavigationErrorf("lazyAttach.%s must be an array of strings, got %s", key, value.Type())
	}

	if arr.Length() == 0 {
		return nil, directNavigationErrorf("lazyAttach.%s must not be empty", key)
	}

	patterns := make([]string, 0, int(arr.Length()))

	var err error

	arr.ForEach(func(item core.Value, idx int) bool {
		s, ok := item.(values.String)

		switch {
		case !ok:
			err = directNavigationErrorf("lazyAttach.%s[%d] must be a string, got %s", key, idx, item.Type())
		case s == "":
			err = directNavigationErrorf("lazyAttach.%s[%d] must not be empty", key, idx)
		default:
			if _, compileErr := drivers.CompileLazyPattern(s.String()); compileErr != nil {
				err = directNavigationErrorf("lazyAttach.%s[%d]: %s", key, idx, compileErr)
			}
		}

		if err != nil {
			return false
		}

		patterns = append(patterns, s.String())

		return true
	})

	if err != nil {
		return nil, err
	}

	return patterns, nil
}
