package html

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

// parseIsolated parses DOCUMENT() params of lazyTestURL from the given properties.
func parseIsolated(props ...*values.ObjectProperty) (PageLoadParams, error) {
	return newPageLoadParams(values.NewString(lazyTestURL), values.NewObjectWith(props...))
}

func TestIsolatedContextParsed(t *testing.T) {
	cases := []struct {
		props []*values.ObjectProperty
		want  bool
	}{
		{nil, false},
		{[]*values.ObjectProperty{lazyProp("isolatedContext", values.True)}, true},
		{[]*values.ObjectProperty{lazyProp("isolatedContext", values.False)}, false},
		// keepCookies: false and fetchMode: false do not conflict with an own context
		{[]*values.ObjectProperty{lazyProp("isolatedContext", values.True), lazyProp("keepCookies", values.False)}, true},
		{[]*values.ObjectProperty{lazyProp("isolatedContext", values.True), lazyProp("fetchMode", values.False)}, true},
		// isolatedContext: false conflicts with nothing
		{[]*values.ObjectProperty{lazyProp("isolatedContext", values.False), lazyProp("keepCookies", values.True)}, false},
	}

	for i, c := range cases {
		res, err := parseIsolated(c.props...)

		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}

		if res.IsolatedContext != c.want {
			t.Errorf("case %d: IsolatedContext %v, want %v", i, res.IsolatedContext, c.want)
		}
	}
}

// core.Error does not wrap its error, so the errors are told by their text.
func TestIsolatedContextTypeError(t *testing.T) {
	_, err := parseIsolated(lazyProp("isolatedContext", values.NewString("true")))

	if err == nil || !strings.HasPrefix(err.Error(), "invalid type: expected [boolean], but got string") {
		t.Errorf("want a type error, got %v", err)
	}
}

func TestIsolatedContextConflicts(t *testing.T) {
	cases := []struct {
		other *values.ObjectProperty
		text  string
	}{
		{lazyProp("keepCookies", values.True), "keepCookies"},
		{lazyProp("fetchMode", values.True), "fetchMode"},
	}

	for _, c := range cases {
		// the order of the keys in the object does not matter
		for _, props := range [][]*values.ObjectProperty{
			{lazyProp("isolatedContext", values.True), c.other},
			{c.other, lazyProp("isolatedContext", values.True)},
		} {
			_, err := parseIsolated(props...)

			if err == nil || err.Error() != "isolatedContext: cannot be used with "+c.text+": true" {
				t.Errorf("isolatedContext with %s: want an isolatedContext error naming both, got %v", c.text, err)
			}
		}
	}
}

// TestOpenLogsIsolatedContextParseError: a query may swallow the error of DOCUMENT() with '?',
// so a conflicting isolatedContext is also written to the log at warn level, like lazyAttach.
// A wrong type is not, as with directNavigation.
func TestOpenLogsIsolatedContextParseError(t *testing.T) {
	cases := []struct {
		value core.Value
		warn  bool
	}{
		{values.True, true},
		{values.NewString("true"), false},
	}

	for _, c := range cases {
		var buf bytes.Buffer

		ctx := logging.WithContext(context.Background(), logging.Options{Writer: &buf, Level: logging.Level(0)})

		_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(
			lazyProp("driver", values.NewString("cdp")),
			lazyProp("keepCookies", values.True),
			lazyProp("isolatedContext", c.value),
		))

		if err == nil {
			t.Fatalf("%v: want a parse error", c.value)
		}

		out := buf.String()
		warned := strings.Contains(out, `"level":"warn"`) && strings.Contains(out, lazyTestURL) && strings.Contains(out, err.Error())

		if warned != c.warn {
			t.Errorf("%v: warn %v, got %q", c.value, c.warn, out)
		}
	}
}
