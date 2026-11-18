package drivers

import (
	"testing"
)

func TestCompileLazyPattern(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		match   bool
	}{
		// '*' is any run of characters, the pattern is matched against the whole string
		{`*/captcha/*`, `https://shop.example/captcha/?back=https%3A%2F%2Fshop.example%2Fproduct%2F1%2F`, true},
		{`*/captcha/*`, `https://shop.example/product/item_b/1002/`, false},
		{`abc`, `xabc`, false},
		{`abc`, `abcx`, false},
		{`a*`, `a`, true},
		{`*/captcha/*`, `https://shop.example/captcha/`, true},
		{`a?c`, `ac`, false},
		{`a?c`, `abbc`, false},
		{`a?c`, `abc`, true},
		// '?' is exactly one character, '\' makes the next character ordinary
		{`*/catalog/\?page=*`, `https://shop.example/catalog/?page=2`, true},
		{`*/catalog/\?page=*`, `https://shop.example/catalog/xpage=2`, false},
		{`*/catalog/?page=*`, `https://shop.example/catalog/?page=2`, true},
		{`*/catalog/?page=*`, `https://shop.example/catalog/xpage=2`, true},
		{`Скидка 50\*`, `Скидка 50*`, true},
		{`Скидка 50\*`, `Скидка 500`, false},
		{`a\\`, `a\`, true},
		// every other character is ordinary, including the ones gobwas/glob treats specially
		{`Товар [новинка]*`, `Товар [новинка] купить`, true},
		{`x{`, `x{`, true},
		{`x{`, `x`, false},
		{`{a,b}`, `a`, false},
		{`a.c`, `abc`, false},
		// case-sensitive and against the raw string, without decoding %XX
		{`*/CAPTCHA/*`, `https://shop.example/captcha/?back=1`, false},
		{`*/%D0%BF*`, `https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/`, true},
		{`*/п*`, `https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/`, false},
		// '*' crosses line breaks too
		{`a*b`, "a\nb", true},
	}

	for _, c := range cases {
		re, err := CompileLazyPattern(c.pattern)

		if err != nil {
			t.Fatalf("pattern %q: unexpected error %v", c.pattern, err)
		}

		if got := re.MatchString(c.input); got != c.match {
			t.Errorf("pattern %q on %q: got %v, want %v", c.pattern, c.input, got, c.match)
		}
	}
}

func TestCompileLazyPatternTrailingBackslash(t *testing.T) {
	for _, p := range []string{`a\`, `*\`, `\`, `a\\\`} {
		if _, err := CompileLazyPattern(p); err == nil {
			t.Errorf("pattern %q: want an error for an unpaired trailing backslash", p)
		}
	}
}
