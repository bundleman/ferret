package html

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

const lazyTestURL = "https://shop.example/product/cup_holder/1001/"

func lazyStrings(items ...string) *values.Array {
	arr := values.NewArray(len(items))

	for _, s := range items {
		arr.Push(values.NewString(s))
	}

	return arr
}

// parseLazy parses DOCUMENT() params of lazyTestURL with the given directNavigation value.
func parseLazy(directNavigation core.Value, extra ...*values.ObjectProperty) (PageLoadParams, error) {
	props := append([]*values.ObjectProperty{values.NewObjectProperty("directNavigation", directNavigation)}, extra...)

	return newPageLoadParams(values.NewString(lazyTestURL), values.NewObjectWith(props...))
}

func lazyAttachObj(props ...*values.ObjectProperty) *values.Object {
	return values.NewObjectWith(values.NewObjectProperty("lazyAttach", values.NewObjectWith(props...)))
}

func Test_NewPageLoadParams_DirectNavigation(t *testing.T) {
	type args struct {
		directNavigation core.Value
		extra            []*values.ObjectProperty
	}
	title := values.NewObjectProperty("title", lazyStrings("*"))
	callTimeout := func(ms int) []*values.ObjectProperty {
		return []*values.ObjectProperty{values.NewObjectProperty("timeout", values.NewInt(ms))}
	}
	tests := []struct {
		name    string
		args    args
		wantErr error
		assert  func(res PageLoadParams)
	}{
		{
			name: "Should keep directNavigation: true as before",
			args: args{directNavigation: values.True},
			assert: func(res PageLoadParams) {
				So(res.DirectNavigation, ShouldBeTrue)
				So(res.LazyAttach, ShouldBeNil)
			},
		},
		{
			name: "Should keep directNavigation: false as before",
			args: args{directNavigation: values.False},
			assert: func(res PageLoadParams) {
				So(res.DirectNavigation, ShouldBeFalse)
				So(res.LazyAttach, ShouldBeNil)
			},
		},
		{
			name: "Should fill lazyAttach defaults",
			args: args{directNavigation: lazyAttachObj(title)},
			assert: func(res PageLoadParams) {
				So(res.DirectNavigation, ShouldBeTrue)
				So(res.LazyAttach, ShouldResemble, &drivers.LazyAttach{
					Timeout:        20000 * time.Millisecond,
					Interval:       300 * time.Millisecond,
					Title:          []string{"*"},
					WaitReadyOnURL: true,
				})
			},
		},
		{
			name: "Should read every lazyAttach field",
			args: args{
				directNavigation: lazyAttachObj(
					values.NewObjectProperty("timeout", values.NewInt(15000)),
					values.NewObjectProperty("interval", values.NewInt(200)),
					values.NewObjectProperty("url", lazyStrings("*/captcha/*")),
					values.NewObjectProperty("title", lazyStrings("*", "Товар [новинка]*")),
					values.NewObjectProperty("waitReadyOnURL", values.False),
				),
				extra: callTimeout(30000),
			},
			assert: func(res PageLoadParams) {
				So(res.LazyAttach, ShouldResemble, &drivers.LazyAttach{
					Timeout:        15000 * time.Millisecond,
					Interval:       200 * time.Millisecond,
					URL:            []string{"*/captcha/*"},
					Title:          []string{"*", "Товар [новинка]*"},
					WaitReadyOnURL: false,
				})
			},
		},
		{
			name:   "Should accept a call timeout above lazyAttach.timeout",
			args:   args{directNavigation: lazyAttachObj(title), extra: callTimeout(60000)},
			assert: func(PageLoadParams) {},
		},
		{
			name:    "Should reject an object without lazyAttach",
			args:    args{directNavigation: values.NewObject()},
			wantErr: errDirectNavigation,
		},
		{
			name: "Should reject an object with another key",
			args: args{directNavigation: values.NewObjectWith(
				values.NewObjectProperty("lazyAttach", values.NewObjectWith(title)),
				values.NewObjectProperty("other", values.True),
			)},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject lazyAttach: null",
			args:    args{directNavigation: values.NewObjectWith(values.NewObjectProperty("lazyAttach", values.None))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject lazyAttach of a boolean",
			args:    args{directNavigation: values.NewObjectWith(values.NewObjectProperty("lazyAttach", values.True))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject neither url nor title",
			args:    args{directNavigation: lazyAttachObj()},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject an empty url",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", lazyStrings()), title)},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject an empty title",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("title", lazyStrings()))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject an empty string in url",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", lazyStrings("*/captcha/*", "")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject an empty string in title",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("title", lazyStrings("")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject an unknown key",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("cookie", values.NewString("PHPSESSID")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject timeout of a string",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("timeout", values.NewString("20000")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject timeout of a float",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("timeout", values.NewFloat(1.5)))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject interval of a string",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("interval", values.NewString("300")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject url that is not an array",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", values.NewString("*/captcha/*")))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject url holding a number",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", values.NewArrayWith(values.NewInt(1))))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject title holding a boolean",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("title", values.NewArrayWith(values.True)))},
			wantErr: errDirectNavigation,
		},
		{
			name: "Should reject waitReadyOnURL of a string",
			args: args{directNavigation: lazyAttachObj(
				values.NewObjectProperty("url", lazyStrings("*/captcha/*")),
				values.NewObjectProperty("waitReadyOnURL", values.NewString("false")),
			)},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject waitReadyOnURL without url",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("waitReadyOnURL", values.False))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject a zero timeout",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("timeout", values.NewInt(0)))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject a negative interval",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("interval", values.NewInt(-1)))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject the default interval greater than timeout",
			args:    args{directNavigation: lazyAttachObj(title, values.NewObjectProperty("timeout", values.NewInt(200)))},
			wantErr: errDirectNavigation,
		},
		{
			name: "Should reject interval greater than timeout",
			args: args{directNavigation: lazyAttachObj(title,
				values.NewObjectProperty("timeout", values.NewInt(300)),
				values.NewObjectProperty("interval", values.NewInt(500)),
			)},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject the default timeout not less than the call timeout",
			args:    args{directNavigation: lazyAttachObj(title), extra: callTimeout(15000)},
			wantErr: errDirectNavigation,
		},
		{
			name: "Should reject timeout equal to the call timeout",
			args: args{
				directNavigation: lazyAttachObj(title, values.NewObjectProperty("timeout", values.NewInt(30000))),
				extra:            callTimeout(30000),
			},
			wantErr: errDirectNavigation,
		},
		{
			name: "Should reject timeout equal to the default call timeout",
			args: args{directNavigation: lazyAttachObj(title,
				values.NewObjectProperty("timeout", values.NewInt(drivers.DefaultPageLoadTimeout)),
			)},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject a url pattern ending with an unpaired backslash",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", lazyStrings(`a\`)))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject a title pattern ending with an unpaired backslash",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("title", lazyStrings(`*\`)))},
			wantErr: errDirectNavigation,
		},
		{
			name:    "Should reject a url pattern matching the requested address",
			args:    args{directNavigation: lazyAttachObj(values.NewObjectProperty("url", lazyStrings("*/product/*")))},
			wantErr: errDirectNavigation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				res, err := parseLazy(tt.args.directNavigation, tt.args.extra...)
				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)

					return
				}
				So(err, ShouldBeNil)
				tt.assert(res)
			})
		})
	}
}

// A value that is neither a boolean nor an object keeps the invalid type error of r.8. It is not a
// lazyAttach error, so it is not marked for the warn record.
func Test_NewPageLoadParams_DirectNavigationWrongType(t *testing.T) {
	tests := []struct {
		name  string
		value core.Value
	}{
		{
			name:  "Should reject a string with an invalid type error",
			value: values.NewString("yes"),
		},
		{
			name:  "Should reject an int with an invalid type error",
			value: values.NewInt(1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				_, err := parseLazy(tt.value)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "invalid type: expected boolean or object, but got "+tt.value.Type().String())
				So(errors.Is(err, errDirectNavigation), ShouldBeFalse)
			})
		})
	}
}

func Test_NewPageLoadParams_LazyAttachBlankDocumentURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			name: "Should reject an empty document URL",
			url:  "",
		},
		{
			name: "Should reject about:blank",
			url:  "about:blank",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				_, err := newPageLoadParams(values.NewString(tt.url), values.NewObjectWith(
					values.NewObjectProperty("directNavigation", lazyAttachObj(values.NewObjectProperty("title", lazyStrings("*")))),
				))
				So(errors.Is(err, errDirectNavigation), ShouldBeTrue)
			})
		})
	}
}

// A query may swallow the error of DOCUMENT() with '?', so a broken lazyAttach is also written to the
// log at warn level. Other parse errors are not.
func Test_Open_LogsDirectNavigationParseError(t *testing.T) {
	tests := []struct {
		name     string
		props    []*values.ObjectProperty
		wantWarn bool
		// wantErrText tells the parse error of a case without a warn record from any other error of Open.
		wantErrText string
	}{
		{
			name: "Should write a warn record with the URL and the error for a broken lazyAttach",
			props: []*values.ObjectProperty{
				values.NewObjectProperty("driver", values.NewString("cdp")),
				values.NewObjectProperty("directNavigation", lazyAttachObj(
					values.NewObjectProperty("title", lazyStrings("*")),
					values.NewObjectProperty("interval", values.NewInt(0)),
				)),
			},
			wantWarn: true,
		},
		{
			name:        "Should not write a warn record for directNavigation of another type",
			props:       []*values.ObjectProperty{values.NewObjectProperty("directNavigation", values.NewString("yes"))},
			wantErrText: "invalid type",
		},
		{
			name:        "Should not write a warn record for an error outside directNavigation",
			props:       []*values.ObjectProperty{values.NewObjectProperty("timeout", values.NewString("x"))},
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

				_, err := Open(ctx, values.NewString(lazyTestURL), values.NewObjectWith(tt.props...))
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
