package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MontFerret/ferret/pkg/drivers"
)

func TestDecodePercent(t *testing.T) {
	cases := []struct{ in, out string }{
		{`%D0%BF`, `п`},
		{`a%20b`, `a b`},
		{`%2F%3F%25`, `/?%`},
		// a '%' without two hex digits stays as is, '+' is not a space
		{`Скидка 50% на смеситель`, `Скидка 50% на смеситель`},
		{`a+b`, `a+b`},
		{`%zz`, `%zz`},
		{`%4`, `%4`},
		{`%`, `%`},
		// one pass: the result is not decoded again
		{`%2541`, `%41`},
		{`%d0%bf`, `п`},
	}

	for _, c := range cases {
		if got := decodePercent(c.in); got != c.out {
			t.Errorf("decodePercent(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestTabAddressesPlaceholder(t *testing.T) {
	type poll struct {
		url         string
		title       string
		placeholder bool
	}

	cases := []struct {
		name      string
		requested string
		polls     []poll
	}{
		{
			name:      "fragment dropped by a redirect, the title keeps the first address",
			requested: "https://shop.example/product/p1/p2/#tab",
			polls: []poll{
				{"https://shop.example/product/p1/p2/#tab", "", true},
				{"https://shop.example/product/p1/p2/#tab", "shop.example/product/p1/p2/#tab", true},
				{"https://shop.example/product/p1/p2/", "shop.example/product/p1/p2/#tab", true},
				{"https://shop.example/product/p1/p2/", "404 Not Found", false},
			},
		},
		{
			name:      "Chrome decodes only some sequences in the placeholder",
			requested: "https://shop.example/a%2Fb%3Fc%25d%20e/",
			polls: []poll{
				{"https://shop.example/a%2Fb%3Fc%25d%20e/", "shop.example/a%2Fb%3Fc%25d e/", true},
			},
		},
		{
			name:      "cyrillic path",
			requested: "https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/",
			polls: []poll{
				{"https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/", "shop.example/поиск/", true},
				{"https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/", "Поиск по каталогу", false},
			},
		},
		{
			name:      "the whole address as the title",
			requested: "https://shop.example/product/p8/p9/",
			polls: []poll{
				{"https://shop.example/product/p8/p9/", "https://shop.example/product/p8/p9/", true},
			},
		},
		{
			name:      "host cut at the query of an address without a path",
			requested: "https://shop.example?x=1",
			polls: []poll{
				{"https://shop.example?x=1", "shop.example", true},
			},
		},
		{
			name:      "site root shows the bare host",
			requested: "https://shop.example/",
			polls: []poll{
				{"https://shop.example/", "shop.example", true},
			},
		},
		{
			name:      "protection step and canonical redirect keep the placeholder of the first address",
			requested: "https://shop.example/product/p3/p4/",
			polls: []poll{
				{"https://shop.example/product/p3/p4/", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3/p4/?step=1a2b", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3_full/p4/?step=1a2b", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3_full/p4/?step=1a2b", "Товар 3", false},
			},
		},
	}

	for _, c := range cases {
		addrs := newTabAddresses(c.requested)

		for i, p := range c.polls {
			addrs.add(p.url)

			if got := addrs.isPlaceholder(p.title); got != p.placeholder {
				t.Errorf("%s, poll %d (%q, %q): placeholder %v, want %v", c.name, i, p.url, p.title, got, p.placeholder)
			}
		}
	}
}

func TestTabAddressesSkipsEmptyURL(t *testing.T) {
	addrs := newTabAddresses("https://shop.example/product/1/")
	addrs.add("")

	if !addrs.isPlaceholder("shop.example/product/1/") {
		t.Errorf("the placeholder of the requested address must stay a placeholder")
	}

	if addrs.isPlaceholder("Товар 1") {
		t.Errorf("a real title must not be a placeholder")
	}
}

// TestLazyVerdictRecordedTabs replays tab sequences recorded on a test stand and in production
// (testdata/lazy_attach_tabs.jsonl, host and paths replaced) through awaitLazyVerdict, one poll per
// record. The real verdict is the captcha address or a title with a space that does not start with
// the host. The wait must end on exactly that poll, never earlier and never later.
func TestLazyVerdictRecordedTabs(t *testing.T) {
	f, err := os.Open("testdata/lazy_attach_tabs.jsonl")

	if err != nil {
		t.Fatal(err)
	}

	defer f.Close()

	isVerdict := func(u, title string) bool {
		return strings.Contains(u, "/captcha/") || strings.Contains(title, " ") && !strings.HasPrefix(title, "shop.example")
	}

	opts := &drivers.LazyAttach{Timeout: 10 * time.Second, Interval: time.Microsecond, URL: []string{"*/captcha/*"}, Title: []string{"*"}}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	tabs := 0

	for scanner.Scan() {
		var tab struct {
			Requested string      `json:"requested"`
			Polls     [][2]string `json:"polls"`
		}

		if err := json.Unmarshal(scanner.Bytes(), &tab); err != nil {
			t.Fatal(err)
		}

		want := -1

		for i, p := range tab.Polls {
			if isVerdict(p[0], p[1]) {
				want = i + 1

				break
			}
		}

		// a tab without a verdict would only wait for the timeout
		if want < 0 {
			continue
		}

		states := make([]tabState, 0, len(tab.Polls))

		for _, p := range tab.Polls {
			states = append(states, tabState{url: p[0], title: p[1]})
		}

		scripted := &scriptedTab{states: states}

		if _, err := awaitLazyVerdict(context.Background(), opts, tab.Requested, scripted.poll); err != nil {
			t.Fatal(err)
		}

		if got := len(scripted.polls()); got != want {
			t.Errorf("tab %q: verdict on poll %d, want %d (%v)", tab.Requested, got, want, tab.Polls)
		}

		tabs++
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if tabs < 100 {
		t.Fatalf("only %d recorded tabs with a verdict", tabs)
	}
}

// TestTabAddressesHostWithBadEscape: the host form is taken without url.Parse, which rejects an
// unpaired '%' in the path, so the placeholder of such an address is still recognised.
func TestTabAddressesHostWithBadEscape(t *testing.T) {
	addrs := newTabAddresses("https://shop.example/50%off/")

	for _, title := range []string{"shop.example", "shop.example/50%off/"} {
		if !addrs.isPlaceholder(title) {
			t.Errorf("%q must be a placeholder of https://shop.example/50%%off/", title)
		}
	}
}
