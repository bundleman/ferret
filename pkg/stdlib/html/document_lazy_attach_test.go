package html

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

const lazyTestURL = "https://shop.example/product/cup_holder/1001/"

func lazyProp(name string, value core.Value) *values.ObjectProperty {
	return values.NewObjectProperty(name, value)
}

func lazyStrings(items ...string) *values.Array {
	arr := values.NewArray(len(items))

	for _, s := range items {
		arr.Push(values.NewString(s))
	}

	return arr
}

// parseLazy parses DOCUMENT() params of lazyTestURL with the given directNavigation value.
func parseLazy(directNavigation core.Value, extra ...*values.ObjectProperty) (PageLoadParams, error) {
	props := append([]*values.ObjectProperty{lazyProp("directNavigation", directNavigation)}, extra...)

	return newPageLoadParams(values.NewString(lazyTestURL), values.NewObjectWith(props...))
}

func lazyAttachObj(props ...*values.ObjectProperty) *values.Object {
	return values.NewObjectWith(lazyProp("lazyAttach", values.NewObjectWith(props...)))
}

func TestDirectNavigationBooleanUnchanged(t *testing.T) {
	for _, v := range []values.Boolean{values.True, values.False} {
		res, err := parseLazy(v)

		if err != nil {
			t.Fatalf("directNavigation %v: %v", v, err)
		}

		if res.DirectNavigation != bool(v) || res.LazyAttach != nil {
			t.Errorf("directNavigation %v: got DirectNavigation %v, LazyAttach %+v", v, res.DirectNavigation, res.LazyAttach)
		}
	}
}

func TestLazyAttachDefaults(t *testing.T) {
	res, err := parseLazy(lazyAttachObj(lazyProp("title", lazyStrings("*"))))

	if err != nil {
		t.Fatal(err)
	}

	want := &drivers.LazyAttach{
		Timeout:        20000 * time.Millisecond,
		Interval:       300 * time.Millisecond,
		Title:          []string{"*"},
		WaitReadyOnURL: true,
	}

	if !res.DirectNavigation || !reflect.DeepEqual(res.LazyAttach, want) {
		t.Errorf("got DirectNavigation %v, LazyAttach %+v, want %+v", res.DirectNavigation, res.LazyAttach, want)
	}
}

func TestLazyAttachAllFields(t *testing.T) {
	res, err := parseLazy(lazyAttachObj(
		lazyProp("timeout", values.NewInt(15000)),
		lazyProp("interval", values.NewInt(200)),
		lazyProp("url", lazyStrings("*/captcha/*")),
		lazyProp("title", lazyStrings("*", "Товар [новинка]*")),
		lazyProp("waitReadyOnURL", values.False),
	), lazyProp("timeout", values.NewInt(30000)))

	if err != nil {
		t.Fatal(err)
	}

	want := &drivers.LazyAttach{
		Timeout:        15000 * time.Millisecond,
		Interval:       200 * time.Millisecond,
		URL:            []string{"*/captcha/*"},
		Title:          []string{"*", "Товар [новинка]*"},
		WaitReadyOnURL: false,
	}

	if !reflect.DeepEqual(res.LazyAttach, want) {
		t.Errorf("got %+v, want %+v", res.LazyAttach, want)
	}
}

func TestLazyAttachParseErrors(t *testing.T) {
	title := lazyProp("title", lazyStrings("*"))

	cases := []struct {
		name  string
		value core.Value
		extra []*values.ObjectProperty
	}{
		{"object without lazyAttach", values.NewObject(), nil},
		{"object with another key", values.NewObjectWith(
			lazyProp("lazyAttach", values.NewObjectWith(title)), lazyProp("other", values.True)), nil},
		{"lazyAttach is null", values.NewObjectWith(lazyProp("lazyAttach", values.None)), nil},
		{"lazyAttach is a boolean", values.NewObjectWith(lazyProp("lazyAttach", values.True)), nil},
		{"neither url nor title", lazyAttachObj(), nil},
		{"empty url", lazyAttachObj(lazyProp("url", lazyStrings()), title), nil},
		{"empty title", lazyAttachObj(lazyProp("title", lazyStrings())), nil},
		{"empty string in url", lazyAttachObj(lazyProp("url", lazyStrings("*/captcha/*", ""))), nil},
		{"empty string in title", lazyAttachObj(lazyProp("title", lazyStrings(""))), nil},
		{"unknown key", lazyAttachObj(title, lazyProp("cookie", values.NewString("PHPSESSID"))), nil},
		{"timeout is a string", lazyAttachObj(title, lazyProp("timeout", values.NewString("20000"))), nil},
		{"timeout is a float", lazyAttachObj(title, lazyProp("timeout", values.NewFloat(1.5))), nil},
		{"interval is a string", lazyAttachObj(title, lazyProp("interval", values.NewString("300"))), nil},
		{"url is not an array", lazyAttachObj(lazyProp("url", values.NewString("*/captcha/*"))), nil},
		{"url holds a number", lazyAttachObj(lazyProp("url", values.NewArrayWith(values.NewInt(1)))), nil},
		{"title holds a boolean", lazyAttachObj(lazyProp("title", values.NewArrayWith(values.True))), nil},
		{"waitReadyOnURL is a string", lazyAttachObj(lazyProp("url", lazyStrings("*/captcha/*")),
			lazyProp("waitReadyOnURL", values.NewString("false"))), nil},
		{"waitReadyOnURL without url", lazyAttachObj(title, lazyProp("waitReadyOnURL", values.False)), nil},
		{"zero timeout", lazyAttachObj(title, lazyProp("timeout", values.NewInt(0))), nil},
		{"negative interval", lazyAttachObj(title, lazyProp("interval", values.NewInt(-1))), nil},
		{"default interval greater than timeout", lazyAttachObj(title, lazyProp("timeout", values.NewInt(200))), nil},
		{"interval greater than timeout", lazyAttachObj(title,
			lazyProp("timeout", values.NewInt(300)), lazyProp("interval", values.NewInt(500))), nil},
		{"default timeout not less than the call timeout", lazyAttachObj(title),
			[]*values.ObjectProperty{lazyProp("timeout", values.NewInt(15000))}},
		{"timeout equal to the call timeout", lazyAttachObj(title, lazyProp("timeout", values.NewInt(30000))),
			[]*values.ObjectProperty{lazyProp("timeout", values.NewInt(30000))}},
		{"timeout equal to the default call timeout", lazyAttachObj(title,
			lazyProp("timeout", values.NewInt(drivers.DefaultPageLoadTimeout))), nil},
		{"url pattern ends with an unpaired backslash", lazyAttachObj(lazyProp("url", lazyStrings(`a\`))), nil},
		{"title pattern ends with an unpaired backslash", lazyAttachObj(lazyProp("title", lazyStrings(`*\`))), nil},
		{"url pattern matches the requested address", lazyAttachObj(lazyProp("url", lazyStrings("*/product/*"))), nil},
	}

	for _, c := range cases {
		_, err := parseLazy(c.value, c.extra...)

		if err == nil {
			t.Errorf("%s: want an error", c.name)

			continue
		}

		if !errors.Is(err, errDirectNavigation) {
			t.Errorf("%s: the error %q is not marked as a directNavigation error", c.name, err)
		}
	}
}

// TestDirectNavigationWrongTypeIsTypeError keeps the invalid type error of r.8 (core.TypeError)
// for a value that is neither a boolean nor an object. It is not a lazyAttach error, so it is not
// marked for the warn record.
func TestDirectNavigationWrongTypeIsTypeError(t *testing.T) {
	for _, v := range []core.Value{values.NewString("yes"), values.NewInt(1)} {
		_, err := parseLazy(v)

		if err == nil || !strings.Contains(err.Error(), "invalid type: expected boolean or object, but got "+v.Type().String()) ||
			errors.Is(err, errDirectNavigation) {
			t.Errorf("%v: got %v, want an unmarked invalid type error", v, err)
		}
	}
}

func TestOpenDoesNotLogWrongDirectNavigationType(t *testing.T) {
	var buf bytes.Buffer

	ctx := logging.WithContext(context.Background(), logging.Options{Writer: &buf, Level: logging.Level(0)})

	_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(lazyProp("directNavigation", values.NewString("yes"))))

	if err == nil || strings.Contains(buf.String(), `"level":"warn"`) {
		t.Errorf("got %v and log %q, want an error and no warn record", err, buf.String())
	}
}

func TestLazyAttachAcceptsCallTimeoutAboveIt(t *testing.T) {
	_, err := parseLazy(lazyAttachObj(lazyProp("title", lazyStrings("*"))), lazyProp("timeout", values.NewInt(60000)))

	if err != nil {
		t.Errorf("lazyAttach.timeout 20000 is less than the call timeout 60000: %v", err)
	}
}

func TestLazyAttachBlankDocumentURL(t *testing.T) {
	for _, u := range []string{"", "about:blank"} {
		_, err := newPageLoadParams(values.NewString(u), values.NewObjectWith(
			lazyProp("directNavigation", lazyAttachObj(lazyProp("title", lazyStrings("*")))),
		))

		if err == nil || !errors.Is(err, errDirectNavigation) {
			t.Errorf("document URL %q: want a directNavigation error, got %v", u, err)
		}
	}
}

// TestOpenLogsLazyAttachParseError checks the warn record: a query may swallow the error of
// DOCUMENT() with '?', and a broken lazyAttach would then go unnoticed.
func TestOpenLogsLazyAttachParseError(t *testing.T) {
	var buf bytes.Buffer

	ctx := logging.WithContext(context.Background(), logging.Options{Writer: &buf, Level: logging.Level(0)})

	_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(
		lazyProp("driver", values.NewString("cdp")),
		lazyProp("directNavigation", lazyAttachObj(lazyProp("title", lazyStrings("*")), lazyProp("interval", values.NewInt(0)))),
	))

	if err == nil {
		t.Fatal("want a parse error")
	}

	out := buf.String()

	if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, lazyTestURL) || !strings.Contains(out, err.Error()) {
		t.Errorf("want a warn record with the URL and the error text, got %q", out)
	}
}

func TestOpenDoesNotLogOtherParseErrors(t *testing.T) {
	var buf bytes.Buffer

	ctx := logging.WithContext(context.Background(), logging.Options{Writer: &buf, Level: logging.Level(0)})

	_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(
		lazyProp("timeout", values.NewString("x")),
	))

	if err == nil {
		t.Fatal("want a parse error")
	}

	if strings.Contains(buf.String(), `"level":"warn"`) {
		t.Errorf("an error outside directNavigation must not be logged, got %q", buf.String())
	}
}
