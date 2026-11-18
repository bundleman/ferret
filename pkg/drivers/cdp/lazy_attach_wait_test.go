package cdp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"

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

func TestLazyVerdictFirstPollImmediately(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}}
	start := time.Now()

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 500*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if err != nil {
		t.Fatal(err)
	}

	if d := tab.polls()[0].Sub(start); d > 50*time.Millisecond {
		t.Errorf("the first poll started after %v, want right away", d)
	}

	if v.Reason != verdictTitle || v.Pattern != "*" || v.Title != "Товар 1" {
		t.Errorf("got %+v, want a verdict by title", v)
	}
}

func TestLazyVerdictIntervalFromEndOfPoll(t *testing.T) {
	placeholder := tabState{waitTestURL, "shop.example/product/1/", false}
	tab := &scriptedTab{states: []tabState{placeholder, placeholder, {waitTestURL, "Товар 1", false}}, delay: 50 * time.Millisecond}

	if _, err := awaitLazyVerdict(context.Background(), lazyOpts(2*time.Second, 30*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll); err != nil {
		t.Fatal(err)
	}

	starts := tab.polls()

	if len(starts) != 3 {
		t.Fatalf("got %d polls, want 3", len(starts))
	}

	for i := 1; i < len(starts); i++ {
		// 50 ms of the previous request plus 30 ms of interval
		if d := starts[i].Sub(starts[i-1]); d < 80*time.Millisecond {
			t.Errorf("poll %d started %v after the previous one, want at least 80ms", i, d)
		}
	}
}

func TestLazyVerdictByURL(t *testing.T) {
	tab := &scriptedTab{states: []tabState{
		{waitTestURL, "", false},
		{"https://shop.example/captcha/?back=1", "shop.example/product/1/", false},
	}}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}), waitTestURL, tab.poll)

	if err != nil {
		t.Fatal(err)
	}

	if v.Reason != verdictURL || v.Pattern != "*/captcha/*" || v.URL != "https://shop.example/captcha/?back=1" {
		t.Errorf("got %+v, want a verdict by url", v)
	}
}

// TestLazyVerdictURLBeforeTitle: when both conditions hold in one poll, the verdict is by url,
// since waitReadyOnURL acts only on a verdict by url and a captcha page may have its own title.
func TestLazyVerdictURLBeforeTitle(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{"https://shop.example/captcha/?back=1", "Access Denied", false}}}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}), waitTestURL, tab.poll)

	if err != nil {
		t.Fatal(err)
	}

	if v.Reason != verdictURL || v.Pattern != "*/captcha/*" {
		t.Errorf("got %+v, want a verdict by url", v)
	}
}

func TestLazyVerdictRedirectIsNotAVerdict(t *testing.T) {
	tab := &scriptedTab{states: []tabState{
		{waitTestURL, "shop.example/product/1/", false},
		{"https://shop.example/product/1/?step=1", "shop.example/product/1/", false},
		{"https://shop.example/product/1-canonical/?step=1", "shop.example/product/1/", false},
		{"https://shop.example/product/1-canonical/?step=1", "Товар 1", false},
	}}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, []string{"*/captcha/*"}, []string{"*"}), waitTestURL, tab.poll)

	if err != nil {
		t.Fatal(err)
	}

	if v.Reason != verdictTitle || len(tab.polls()) != 4 {
		t.Errorf("got %+v after %d polls, want a verdict by title on poll 4", v, len(tab.polls()))
	}
}

func TestLazyVerdictSkipsEmptyURL(t *testing.T) {
	tab := &scriptedTab{states: []tabState{
		{"", "Товар 1", false},
		{waitTestURL, "Товар 1", false},
	}}

	if _, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll); err != nil {
		t.Fatal(err)
	}

	if n := len(tab.polls()); n != 2 {
		t.Errorf("verdict after %d polls, want 2: a poll with an empty address is skipped", n)
	}
}

func TestLazyVerdictTimeout(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "shop.example/product/1/", false}}}
	start := time.Now()

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(100*time.Millisecond, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if err != nil {
		t.Fatalf("the end of timeout is not an error, got %v", err)
	}

	if v.Reason != verdictTimeout || time.Since(start) < 100*time.Millisecond {
		t.Errorf("got %+v after %v, want a timeout after 100ms", v, time.Since(start))
	}

	if v.URL != waitTestURL || v.Title != "shop.example/product/1/" {
		t.Errorf("got %+v, want the last address and title in the verdict", v)
	}
}

// TestLazyVerdictTimeoutDuringRequest: a poll still waiting for the browser when timeout ends
// is dropped, the wait ends on time and it is still a timeout, not an error.
func TestLazyVerdictTimeoutDuringRequest(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "", false}}, delay: time.Hour}
	start := time.Now()

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(100*time.Millisecond, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if err != nil || v.Reason != verdictTimeout {
		t.Fatalf("got %+v, %v, want a timeout without an error", v, err)
	}

	if d := time.Since(start); d > time.Second {
		t.Errorf("the wait took %v, want it to end at timeout", d)
	}
}

func TestLazyVerdictPollError(t *testing.T) {
	tab := &scriptedTab{err: rpcc.ErrConnClosing}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if !errors.Is(err, rpcc.ErrConnClosing) || v.Reason != verdictPollError {
		t.Errorf("got %+v, %v, want the poll error as is", v, err)
	}
}

func TestLazyVerdictContextCanceled(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "", false}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	v, err := awaitLazyVerdict(ctx, lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if !errors.Is(err, context.DeadlineExceeded) || v.Reason != verdictCanceled {
		t.Errorf("got %+v, %v, want the context error", v, err)
	}
}

// TestLazyVerdictContextCanceledDuringRequest: the end of the DOCUMENT() deadline during a poll
// request is a canceled context, not a timeout of lazyAttach.
func TestLazyVerdictContextCanceledDuringRequest(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "", false}}, delay: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	v, err := awaitLazyVerdict(ctx, lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if !errors.Is(err, context.DeadlineExceeded) || v.Reason != verdictCanceled {
		t.Errorf("got %+v, %v, want the context error", v, err)
	}
}

func TestLazyVerdictAttachedSeen(t *testing.T) {
	tab := &scriptedTab{states: []tabState{
		{waitTestURL, "", true},
		{waitTestURL, "Товар 1", false},
	}}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}), waitTestURL, tab.poll)

	if err != nil || !v.AttachedSeen {
		t.Errorf("got %+v, %v, want attached seen", v, err)
	}
}

func TestLazyVerdictTitlePatternMustMatch(t *testing.T) {
	tab := &scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}}

	v, err := awaitLazyVerdict(context.Background(), lazyOpts(60*time.Millisecond, 10*time.Millisecond, nil, []string{"404*"}), waitTestURL, tab.poll)

	if err != nil || v.Reason != verdictTimeout {
		t.Errorf("got %+v, %v, want a timeout: the title does not match the pattern", v, err)
	}
}
