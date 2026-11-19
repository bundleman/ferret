package cdp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
)

const waitTestURL = "https://shop.example/product/1/"

// tabState is one answer of the tab poll.
type tabState struct {
	url, title string
	attached   bool
}

// scriptedTab answers polls from a list, repeating the last state, and records poll times.
type scriptedTab struct {
	mu     sync.Mutex
	states []tabState
	delay  time.Duration
	err    error
	starts []time.Time
}

func (s *scriptedTab) poll(ctx context.Context) (string, string, bool, error) {
	s.mu.Lock()
	s.starts = append(s.starts, time.Now())
	n := len(s.starts)
	s.mu.Unlock()

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", "", false, ctx.Err()
		}
	}

	if s.err != nil {
		return "", "", false, s.err
	}

	st := s.states[len(s.states)-1]

	if n <= len(s.states) {
		st = s.states[n-1]
	}

	return st.url, st.title, st.attached, nil
}

func (s *scriptedTab) polls() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]time.Time(nil), s.starts...)
}

func lazyOpts(timeout, interval time.Duration, url, title []string) *drivers.LazyAttach {
	return &drivers.LazyAttach{Timeout: timeout, Interval: interval, URL: url, Title: title, WaitReadyOnURL: true}
}

func Test_AwaitLazyVerdict(t *testing.T) {
	type args struct {
		tab      *scriptedTab
		opts     *drivers.LazyAttach
		deadline time.Duration
	}
	placeholder := tabState{waitTestURL, "shop.example/product/1/", false}
	tests := []struct {
		name    string
		args    args
		wantErr error
		assert  func(v lazyVerdict, tab *scriptedTab, start time.Time)
	}{
		{
			name: "Should poll right away and give a verdict by title",
			args: args{
				tab:  &scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}},
				opts: lazyOpts(time.Second, 500*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(v lazyVerdict, tab *scriptedTab, start time.Time) {
				So(tab.polls()[0].Sub(start), ShouldBeLessThanOrEqualTo, 50*time.Millisecond)
				So(v.Reason, ShouldEqual, verdictTitle)
				So(v.Pattern, ShouldEqual, "*")
				So(v.Title, ShouldEqual, "Товар 1")
			},
		},
		{
			name: "Should count the interval from the end of the previous poll",
			args: args{
				tab:  &scriptedTab{states: []tabState{placeholder, placeholder, {waitTestURL, "Товар 1", false}}, delay: 50 * time.Millisecond},
				opts: lazyOpts(2*time.Second, 30*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(_ lazyVerdict, tab *scriptedTab, _ time.Time) {
				starts := tab.polls()
				So(len(starts), ShouldEqual, 3)

				for i := 1; i < len(starts); i++ {
					// 50 ms of the previous request plus 30 ms of interval
					So(starts[i].Sub(starts[i-1]), ShouldBeGreaterThanOrEqualTo, 80*time.Millisecond)
				}
			},
		},
		{
			name: "Should give a verdict by url",
			args: args{
				tab: &scriptedTab{states: []tabState{
					{waitTestURL, "", false},
					{"https://shop.example/captcha/?back=1", "shop.example/product/1/", false},
				}},
				opts: lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictURL)
				So(v.Pattern, ShouldEqual, "*/captcha/*")
				So(v.URL, ShouldEqual, "https://shop.example/captcha/?back=1")
			},
		},
		{
			// waitReadyOnURL acts only on a verdict by url, and a captcha page may have a title of its own.
			name: "Should check the url before the title in one poll",
			args: args{
				tab:  &scriptedTab{states: []tabState{{"https://shop.example/captcha/?back=1", "Access Denied", false}}},
				opts: lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictURL)
				So(v.Pattern, ShouldEqual, "*/captcha/*")
			},
		},
		{
			name: "Should not take a redirect for a verdict",
			args: args{
				tab: &scriptedTab{states: []tabState{
					{waitTestURL, "shop.example/product/1/", false},
					{"https://shop.example/product/1/?step=1", "shop.example/product/1/", false},
					{"https://shop.example/product/1-canonical/?step=1", "shop.example/product/1/", false},
					{"https://shop.example/product/1-canonical/?step=1", "Товар 1", false},
				}},
				opts: lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}),
			},
			assert: func(v lazyVerdict, tab *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictTitle)
				So(len(tab.polls()), ShouldEqual, 4)
			},
		},
		{
			name: "Should skip a poll with an empty address",
			args: args{
				tab: &scriptedTab{states: []tabState{
					{"", "Товар 1", false},
					{waitTestURL, "Товар 1", false},
				}},
				opts: lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(_ lazyVerdict, tab *scriptedTab, _ time.Time) {
				So(len(tab.polls()), ShouldEqual, 2)
			},
		},
		{
			name: "Should end with a timeout that is not an error and keep the last address and title",
			args: args{
				tab:  &scriptedTab{states: []tabState{placeholder}},
				opts: lazyOpts(100*time.Millisecond, 10*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, start time.Time) {
				So(v.Reason, ShouldEqual, verdictTimeout)
				So(time.Since(start), ShouldBeGreaterThanOrEqualTo, 100*time.Millisecond)
				So(v.URL, ShouldEqual, waitTestURL)
				So(v.Title, ShouldEqual, "shop.example/product/1/")
			},
		},
		{
			name: "Should end on time with a timeout while a poll waits for the browser",
			args: args{
				tab:  &scriptedTab{states: []tabState{{waitTestURL, "", false}}, delay: time.Hour},
				opts: lazyOpts(100*time.Millisecond, 10*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, start time.Time) {
				So(v.Reason, ShouldEqual, verdictTimeout)
				So(time.Since(start), ShouldBeLessThanOrEqualTo, time.Second)
			},
		},
		{
			name: "Should report a tab seen attached",
			args: args{
				tab: &scriptedTab{states: []tabState{
					{waitTestURL, "", true},
					{waitTestURL, "Товар 1", false},
				}},
				opts: lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.AttachedSeen, ShouldBeTrue)
			},
		},
		{
			name: "Should wait for a title that matches the pattern",
			args: args{
				tab:  &scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}},
				opts: lazyOpts(60*time.Millisecond, 10*time.Millisecond, nil, []string{"404*"}),
			},
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictTimeout)
			},
		},
		{
			name: "Should return a poll error as is",
			args: args{
				tab:  &scriptedTab{err: rpcc.ErrConnClosing},
				opts: lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}),
			},
			wantErr: rpcc.ErrConnClosing,
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictPollError)
			},
		},
		{
			name: "Should return the context error when the call ends",
			args: args{
				tab:      &scriptedTab{states: []tabState{{waitTestURL, "", false}}},
				opts:     lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}),
				deadline: 50 * time.Millisecond,
			},
			wantErr: context.DeadlineExceeded,
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictCanceled)
			},
		},
		{
			// The end of the DOCUMENT() deadline is not a timeout of lazyAttach, also during a poll request.
			name: "Should return the context error when the call ends during a poll request",
			args: args{
				tab:      &scriptedTab{states: []tabState{{waitTestURL, "", false}}, delay: time.Hour},
				opts:     lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}),
				deadline: 50 * time.Millisecond,
			},
			wantErr: context.DeadlineExceeded,
			assert: func(v lazyVerdict, _ *scriptedTab, _ time.Time) {
				So(v.Reason, ShouldEqual, verdictCanceled)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				ctx := context.Background()

				if tt.args.deadline > 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, tt.args.deadline)
					t.Cleanup(cancel)
				}

				start := time.Now()
				v, err := awaitLazyVerdict(ctx, tt.args.opts, waitTestURL, tt.args.tab.poll)

				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
				} else {
					So(err, ShouldBeNil)
				}

				tt.assert(v, tt.args.tab, start)
			})
		})
	}
}
