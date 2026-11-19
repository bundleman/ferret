package drivers

import (
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	// DefaultLazyAttachTimeout is how long, in milliseconds, a tab waits for a verdict
	// without a DevTools session when lazyAttach does not set its own timeout.
	DefaultLazyAttachTimeout = 20000
	// DefaultLazyAttachInterval is how often, in milliseconds, the tab is polled.
	DefaultLazyAttachInterval = 300
)

// LazyAttach makes a directNavigation tab wait for a verdict of the page before the driver
// attaches to it: the tab is polled through the browser connection, so no DevTools session lives
// on it while the page loads. The verdict is an address matching a URL pattern or a real title
// (not Chrome's placeholder) matching a Title pattern; without it the wait ends after Timeout.
type LazyAttach struct {
	Timeout  time.Duration
	Interval time.Duration
	URL      []string
	Title    []string
	// WaitReadyOnURL keeps waiting for the end of HTML parsing after a verdict by URL.
	WaitReadyOnURL bool
}

// CompileLazyPattern turns a lazyAttach url or title pattern into a regular expression matched
// against the whole string. '*' is any run of characters, '?' is exactly one character and '\'
// makes the next character ordinary; every other character is ordinary. Unlike
// ignore.resources.url (gobwas/glob), '[' and '{' have no special meaning here, and a pattern
// ending in an unpaired '\' is an error instead of being silently cut.
func CompileLazyPattern(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder

	b.WriteString(`(?s)\A`)

	runes := []rune(pattern)

	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '\\':
			if i+1 == len(runes) {
				return nil, errors.Errorf("pattern %q ends with an unpaired backslash", pattern)
			}

			i++
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}

	b.WriteString(`\z`)

	return regexp.Compile(b.String())
}
