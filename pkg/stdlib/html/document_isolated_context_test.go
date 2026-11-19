package html

import (
	"bytes"
	"context"
	"testing"

	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

func Test_NewPageLoadParams_IsolatedContext(t *testing.T) {
	type args struct {
		props []*values.ObjectProperty
	}
	isolated := values.NewObjectProperty("isolatedContext", values.True)
	keepCookies := values.NewObjectProperty("keepCookies", values.True)
	fetchMode := values.NewObjectProperty("fetchMode", values.True)
	tests := []struct {
		name        string
		args        args
		want        bool
		wantErr     error
		wantErrText string
	}{
		{
			name: "Should be off when omitted",
			args: args{},
			want: false,
		},
		{
			name: "Should be on when true",
			args: args{props: []*values.ObjectProperty{isolated}},
			want: true,
		},
		{
			name: "Should be off when false",
			args: args{props: []*values.ObjectProperty{values.NewObjectProperty("isolatedContext", values.False)}},
			want: false,
		},
		{
			name: "Should allow keepCookies: false",
			args: args{props: []*values.ObjectProperty{isolated, values.NewObjectProperty("keepCookies", values.False)}},
			want: true,
		},
		{
			name: "Should allow fetchMode: false",
			args: args{props: []*values.ObjectProperty{isolated, values.NewObjectProperty("fetchMode", values.False)}},
			want: true,
		},
		{
			name: "Should allow keepCookies: true when isolatedContext is false",
			args: args{props: []*values.ObjectProperty{values.NewObjectProperty("isolatedContext", values.False), keepCookies}},
			want: false,
		},
		{
			name:        "Should reject keepCookies: true",
			args:        args{props: []*values.ObjectProperty{isolated, keepCookies}},
			wantErr:     errIsolatedContext,
			wantErrText: "isolatedContext: cannot be used with keepCookies: true",
		},
		{
			name:        "Should reject keepCookies: true given before it",
			args:        args{props: []*values.ObjectProperty{keepCookies, isolated}},
			wantErr:     errIsolatedContext,
			wantErrText: "isolatedContext: cannot be used with keepCookies: true",
		},
		{
			name:        "Should reject fetchMode: true",
			args:        args{props: []*values.ObjectProperty{isolated, fetchMode}},
			wantErr:     errIsolatedContext,
			wantErrText: "isolatedContext: cannot be used with fetchMode: true",
		},
		{
			name:        "Should reject fetchMode: true given before it",
			args:        args{props: []*values.ObjectProperty{fetchMode, isolated}},
			wantErr:     errIsolatedContext,
			wantErrText: "isolatedContext: cannot be used with fetchMode: true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				res, err := newPageLoadParams(values.NewString(lazyTestURL), values.NewObjectWith(tt.args.props...))
				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
					So(err.Error(), ShouldEqual, tt.wantErrText)

					return
				}
				So(err, ShouldBeNil)
				So(res.IsolatedContext, ShouldEqual, tt.want)
			})
		})
	}
}

// core.TypeError wraps no sentinel, so the error is told by its text.
func Test_NewPageLoadParams_IsolatedContextWrongType(t *testing.T) {
	Convey("Should reject a value of another type", t, func() {
		_, err := newPageLoadParams(values.NewString(lazyTestURL), values.NewObjectWith(
			values.NewObjectProperty("isolatedContext", values.NewString("true")),
		))
		So(err, ShouldNotBeNil)
		So(err.Error(), ShouldContainSubstring, "invalid type: expected [boolean], but got string")
		So(errors.Is(err, errIsolatedContext), ShouldBeFalse)
	})
}

// A query may swallow the error of DOCUMENT() with '?', so a conflicting isolatedContext is also
// written to the log at warn level, as a broken lazyAttach is. A wrong type is not, as with directNavigation.
func Test_Open_LogsIsolatedContextConflict(t *testing.T) {
	tests := []struct {
		name     string
		value    core.Value
		wantWarn bool
		// wantErrText tells the parse error of a case without a warn record from any other error of Open.
		wantErrText string
	}{
		{
			name:     "Should write a warn record for a conflict with keepCookies",
			value:    values.True,
			wantWarn: true,
		},
		{
			name:        "Should not write a warn record for a value of another type",
			value:       values.NewString("true"),
			wantErrText: "invalid type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var buf bytes.Buffer

				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				ctx = logging.WithContext(ctx, logging.Options{Writer: &buf, Level: logging.Level(0)})

				_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(
					values.NewObjectProperty("driver", values.NewString("cdp")),
					values.NewObjectProperty("keepCookies", values.True),
					values.NewObjectProperty("isolatedContext", tt.value),
				))
				So(err, ShouldNotBeNil)

				out := buf.String()
				if !tt.wantWarn {
					So(err.Error(), ShouldContainSubstring, tt.wantErrText)
					So(out, ShouldNotContainSubstring, `"level":"warn"`)

					return
				}
				So(out, ShouldContainSubstring, `"level":"warn"`)
				So(out, ShouldContainSubstring, lazyTestURL)
				So(out, ShouldContainSubstring, err.Error())
			})
		})
	}
}
