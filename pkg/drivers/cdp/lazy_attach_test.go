package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
)

func Test_DecodePercent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "Should decode a cyrillic letter", in: `%D0%BF`, want: `п`},
		{name: "Should decode a space", in: `a%20b`, want: `a b`},
		{name: "Should decode reserved characters", in: `%2F%3F%25`, want: `/?%`},
		{name: "Should keep a '%' without two hex digits", in: `Скидка 50% на смеситель`, want: `Скидка 50% на смеситель`},
		{name: "Should keep '+' as is", in: `a+b`, want: `a+b`},
		{name: "Should keep '%' before non-hex digits", in: `%zz`, want: `%zz`},
		{name: "Should keep '%' before one hex digit", in: `%4`, want: `%4`},
		{name: "Should keep a lone '%'", in: `%`, want: `%`},
		{name: "Should decode in one pass", in: `%2541`, want: `%41`},
		{name: "Should decode lower case hex digits", in: `%d0%bf`, want: `п`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				So([2]string{tt.in, decodePercent(tt.in)}, ShouldResemble, [2]string{tt.in, tt.want})
			})
		})
	}
}

func Test_TabAddresses_IsPlaceholder(t *testing.T) {
	type poll struct {
		url         string
		title       string
		placeholder bool
	}
	tests := []struct {
		name      string
		requested string
		polls     []poll
	}{
		{
			name:      "Should keep the first address when a redirect drops the fragment",
			requested: "https://shop.example/product/p1/p2/#tab",
			polls: []poll{
				{"https://shop.example/product/p1/p2/#tab", "", true},
				{"https://shop.example/product/p1/p2/#tab", "shop.example/product/p1/p2/#tab", true},
				{"https://shop.example/product/p1/p2/", "shop.example/product/p1/p2/#tab", true},
				{"https://shop.example/product/p1/p2/", "404 Not Found", false},
			},
		},
		{
			name:      "Should match a placeholder where Chrome decodes only some sequences",
			requested: "https://shop.example/a%2Fb%3Fc%25d%20e/",
			polls: []poll{
				{"https://shop.example/a%2Fb%3Fc%25d%20e/", "shop.example/a%2Fb%3Fc%25d e/", true},
			},
		},
		{
			name:      "Should match a cyrillic path",
			requested: "https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/",
			polls: []poll{
				{"https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/", "shop.example/поиск/", true},
				{"https://shop.example/%D0%BF%D0%BE%D0%B8%D1%81%D0%BA/", "Поиск по каталогу", false},
			},
		},
		{
			name:      "Should match the whole address as the title",
			requested: "https://shop.example/product/p8/p9/",
			polls: []poll{
				{"https://shop.example/product/p8/p9/", "https://shop.example/product/p8/p9/", true},
			},
		},
		{
			name:      "Should cut the host at the query of an address without a path",
			requested: "https://shop.example?x=1",
			polls: []poll{
				{"https://shop.example?x=1", "shop.example", true},
			},
		},
		{
			name:      "Should match the bare host of the site root",
			requested: "https://shop.example/",
			polls: []poll{
				{"https://shop.example/", "shop.example", true},
			},
		},
		{
			name:      "Should keep the placeholder of the first address through a protection step and a canonical redirect",
			requested: "https://shop.example/product/p3/p4/",
			polls: []poll{
				{"https://shop.example/product/p3/p4/", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3/p4/?step=1a2b", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3_full/p4/?step=1a2b", "shop.example/product/p3/p4/", true},
				{"https://shop.example/product/p3_full/p4/?step=1a2b", "Товар 3", false},
			},
		},
		{
			name:      "Should skip an empty address",
			requested: "https://shop.example/product/1/",
			polls: []poll{
				{"", "shop.example/product/1/", true},
				{"", "Товар 1", false},
			},
		},
		{
			// The host is cut without url.Parse, which rejects an unpaired '%' in the path.
			name:      "Should match the host of an address with an unpaired '%'",
			requested: "https://shop.example/50%off/",
			polls: []poll{
				{"", "shop.example", true},
				{"", "shop.example/50%off/", true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				addrs := newTabAddresses(tt.requested)
				got := make([][2]interface{}, 0, len(tt.polls))
				want := make([][2]interface{}, 0, len(tt.polls))

				for _, p := range tt.polls {
					addrs.add(p.url)

					got = append(got, [2]interface{}{p.title, addrs.isPlaceholder(p.title)})
					want = append(want, [2]interface{}{p.title, p.placeholder})
				}

				So(got, ShouldResemble, want)
			})
		})
	}
}

// The tab sequences were recorded on a test stand and in production (host and paths replaced). The
// real verdict is the captcha address or a title with a space that does not start with the host, and
// the wait must end on exactly that poll, never earlier and never later.
func Test_AwaitLazyVerdict_RecordedTabs(t *testing.T) {
	Convey("Should end the wait on the poll of the real verdict in every recorded tab", t, func() {
		f, err := os.Open("testdata/lazy_attach_tabs.jsonl")
		So(err, ShouldBeNil)
		t.Cleanup(func() { _ = f.Close() })

		isVerdict := func(u, title string) bool {
			return strings.Contains(u, "/captcha/") || strings.Contains(title, " ") && !strings.HasPrefix(title, "shop.example")
		}

		// A scripted tab answers in microseconds, so a missed verdict ends at this timeout instead of hanging the test.
		opts := &drivers.LazyAttach{Timeout: time.Second, Interval: time.Microsecond, URL: []string{"*/captcha/*"}, Title: []string{"*"}}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		tabs, line := 0, 0

		for scanner.Scan() {
			line++

			var tab struct {
				Requested string      `json:"requested"`
				Polls     [][2]string `json:"polls"`
			}

			So(json.Unmarshal(scanner.Bytes(), &tab), ShouldBeNil)

			verdictPoll := -1

			for i, p := range tab.Polls {
				if isVerdict(p[0], p[1]) {
					verdictPoll = i + 1

					break
				}
			}

			// a tab without a verdict would only wait for the timeout
			if verdictPoll < 0 {
				continue
			}

			states := make([]tabState, 0, len(tab.Polls))

			for _, p := range tab.Polls {
				states = append(states, tabState{url: p[0], title: p[1]})
			}

			scripted := &scriptedTab{states: states}

			_, err := awaitLazyVerdict(context.Background(), opts, tab.Requested, scripted.poll)
			errText := ""

			if err != nil {
				errText = err.Error()
			}

			// line of the record, error text and number of polls
			So([3]interface{}{line, errText, len(scripted.polls())}, ShouldResemble, [3]interface{}{line, "", verdictPoll})

			tabs++
		}

		So(scanner.Err(), ShouldBeNil)
		So(tabs, ShouldBeGreaterThanOrEqualTo, 100)
	})
}
