package drivers

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func Test_CompileLazyPattern_Match(t *testing.T) {
	type args struct {
		pattern string
		input   string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "Should match any run of characters with '*'",
			args: args{pattern: `*/captcha/*`, input: `https://shop.example/captcha/?back=https%3A%2F%2Fshop.example%2Fproduct%2F1%2F`},
			want: true,
		},
		{
			name: "Should not match an address without the fixed part",
			args: args{pattern: `*/captcha/*`, input: `https://shop.example/product/item_b/1002/`},
			want: false,
		},
		{
			name: "Should match the whole string, not a suffix",
			args: args{pattern: `abc`, input: `xabc`},
			want: false,
		},
		{
			name: "Should match the whole string, not a prefix",
			args: args{pattern: `abc`, input: `abcx`},
			want: false,
		},
		{
			name: "Should match an empty run with a trailing '*'",
			args: args{pattern: `a*`, input: `a`},
			want: true,
		},
		{
			name: "Should match an empty run with '*' at the end of an address",
			args: args{pattern: `*/captcha/*`, input: `https://shop.example/captcha/`},
			want: true,
		},
		{
			name: "Should not match zero characters with '?'",
			args: args{pattern: `a?c`, input: `ac`},
			want: false,
		},
		{
			name: "Should not match two characters with '?'",
			args: args{pattern: `a?c`, input: `abbc`},
			want: false,
		},
		{
			name: "Should match exactly one character with '?'",
			args: args{pattern: `a?c`, input: `abc`},
			want: true,
		},
		{
			name: "Should match an escaped '?' as itself",
			args: args{pattern: `*/catalog/\?page=*`, input: `https://shop.example/catalog/?page=2`},
			want: true,
		},
		{
			name: "Should not match another character with an escaped '?'",
			args: args{pattern: `*/catalog/\?page=*`, input: `https://shop.example/catalog/xpage=2`},
			want: false,
		},
		{
			name: "Should match '?' with an unescaped '?'",
			args: args{pattern: `*/catalog/?page=*`, input: `https://shop.example/catalog/?page=2`},
			want: true,
		},
		{
			name: "Should match another character with an unescaped '?'",
			args: args{pattern: `*/catalog/?page=*`, input: `https://shop.example/catalog/xpage=2`},
			want: true,
		},
		{
			name: "Should match an escaped '*' as itself",
			args: args{pattern: `Скидка 50\*`, input: `Скидка 50*`},
			want: true,
		},
		{
			name: "Should not match another character with an escaped '*'",
			args: args{pattern: `Скидка 50\*`, input: `Скидка 500`},
			want: false,
		},
		{
			name: "Should match an escaped backslash as itself",
			args: args{pattern: `a\\`, input: `a\`},
			want: true,
		},
		{
			name: "Should treat '[' and ']' as ordinary characters",
			args: args{pattern: `Товар [новинка]*`, input: `Товар [новинка] купить`},
			want: true,
		},
		{
			name: "Should treat an unpaired '{' as an ordinary character",
			args: args{pattern: `x{`, input: `x{`},
			want: true,
		},
		{
			name: "Should not drop an unpaired '{'",
			args: args{pattern: `x{`, input: `x`},
			want: false,
		},
		{
			name: "Should not treat '{a,b}' as alternatives",
			args: args{pattern: `{a,b}`, input: `a`},
			want: false,
		},
		{
			name: "Should treat '.' as an ordinary character",
			args: args{pattern: `a.c`, input: `abc`},
			want: false,
		},
		{
			name: "Should match case-sensitively",
			args: args{pattern: `*/CAPTCHA/*`, input: `https://shop.example/captcha/?back=1`},
			want: false,
		},
		{
			name: "Should match %XX sequences as they are",
			args: args{pattern: `*/%D0%BF*`, input: `https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/`},
			want: true,
		},
		{
			name: "Should not decode %XX sequences of the input",
			args: args{pattern: `*/п*`, input: `https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/`},
			want: false,
		},
		{
			name: "Should match line breaks with '*'",
			args: args{pattern: `a*b`, input: "a\nb"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				re, err := CompileLazyPattern(tt.args.pattern)
				So(err, ShouldBeNil)
				t.Logf("pattern %q, input %q", tt.args.pattern, tt.args.input)
				So(re.MatchString(tt.args.input), ShouldEqual, tt.want)
			})
		})
	}
}

func Test_CompileLazyPattern_TrailingBackslash(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{
			name:    "Should reject a backslash after a character",
			pattern: `a\`,
		},
		{
			name:    "Should reject a backslash after '*'",
			pattern: `*\`,
		},
		{
			name:    "Should reject a lone backslash",
			pattern: `\`,
		},
		{
			name:    "Should reject an unpaired backslash after an escaped one",
			pattern: `a\\\`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				_, err := CompileLazyPattern(tt.pattern)
				So(err, ShouldNotBeNil)
				So(err.Error(), ShouldContainSubstring, "ends with an unpaired backslash")
			})
		})
	}
}
