package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"
	"github.com/rs/zerolog"

	"github.com/MontFerret/ferret/pkg/drivers"
)

func logReport(t *testing.T, report *lazyAttachReport, err error) map[string]interface{} {
	t.Helper()

	var buf bytes.Buffer

	report.log(zerolog.New(&buf).Level(zerolog.DebugLevel), err)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))

	if len(lines) != 1 {
		t.Fatalf("want one record, got %q", buf.String())
	}

	var rec map[string]interface{}

	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatal(err)
	}

	return rec
}

func checkFields(t *testing.T, rec map[string]interface{}, want map[string]interface{}) {
	t.Helper()

	for k, v := range want {
		if rec[k] != v {
			t.Errorf("field %s = %v, want %v (record %v)", k, rec[k], v, rec)
		}
	}
}

func TestLazyAttachLogSuccess(t *testing.T) {
	report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
	report.setIncognito(false)
	report.setTarget("T1")
	report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*", URL: waitTestURL, Title: "Товар 1", Wait: 4730 * time.Millisecond})
	report.setCommitted(true)
	report.setParsed("ready", 150*time.Millisecond)

	rec := logReport(t, report, nil)

	checkFields(t, rec, map[string]interface{}{
		"level":           "info",
		"target_id":       "T1",
		"incognito":       false,
		"url":             waitTestURL,
		"verdict":         "title",
		"pattern":         "*",
		"verdict_url":     waitTestURL,
		"verdict_title":   "Товар 1",
		"attached_seen":   false,
		"verdict_wait_ms": float64(4730),
		"committed":       "yes",
		"parsed":          "ready",
		"parsed_wait_ms":  float64(150),
		"result":          "ok",
		"stage":           "",
		"error":           "",
	})
}

// TestLazyAttachLogBeforePoll: a call that failed before the first poll is written too, with
// every step it did not come to marked as not reached.
func TestLazyAttachLogBeforePoll(t *testing.T) {
	report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
	cause := errors.New("failed to initialize driver")
	report.fail(stagePrepare, cause)

	rec := logReport(t, report, cause)

	checkFields(t, rec, map[string]interface{}{
		"verdict":   "before_poll",
		"committed": "not_reached",
		"parsed":    "not_reached",
		"result":    "error",
		"stage":     "prepare",
		"error":     "failed to initialize driver",
	})
}

func TestLazyAttachLogPollError(t *testing.T) {
	opener, _, _ := newTestOpener(&scriptedTab{err: rpcc.ErrConnClosing})
	report := newLazyAttachReport(lazyParams(nil))

	_, err := opener.open(context.Background(), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

	rec := logReport(t, report, err)

	checkFields(t, rec, map[string]interface{}{
		"target_id": "T1",
		"incognito": true,
		"verdict":   "poll_error",
		"committed": "not_reached",
		"parsed":    "not_reached",
		"result":    "error",
		"stage":     "poll",
		"error":     rpcc.ErrConnClosing.Error(),
	})
}

func TestLazyAttachLogSkippedParsing(t *testing.T) {
	report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
	report.setVerdict(lazyVerdict{Reason: verdictURL, Pattern: "*/captcha/*"})
	report.setCommitted(true)
	report.setParsed("skipped", 0)

	checkFields(t, logReport(t, report, nil), map[string]interface{}{"verdict": "url", "parsed": "skipped", "result": "ok"})
}

// TestLazyAttachLogUnmarkedError: an error the steps did not mark still gets into the record.
func TestLazyAttachLogUnmarkedError(t *testing.T) {
	report := newLazyAttachReport(drivers.Params{URL: waitTestURL})

	checkFields(t, logReport(t, report, errors.New("boom")), map[string]interface{}{
		"result": "error", "stage": "unknown", "error": "boom",
	})
}

func TestLazyAttachLogNilReport(t *testing.T) {
	var buf bytes.Buffer
	var report *lazyAttachReport

	report.log(zerolog.New(&buf), errors.New("boom"))

	if buf.Len() != 0 {
		t.Errorf("a call without lazyAttach must not be written, got %q", buf.String())
	}
}

// TestFetchPoolHoldsSlotWhileOpening: a new fetchMode tab with lazyAttach waits for its verdict
// inside the open callback, and the pool keeps the place of that tab until it returns.
func TestFetchPoolHoldsSlotWhileOpening(t *testing.T) {
	pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
	var opened int32
	opening := make(chan struct{})
	finish := make(chan struct{})

	open := func(_ context.Context) (fetchTab, error) {
		if atomic.AddInt32(&opened, 1) == 1 {
			close(opening)
		}

		<-finish

		return &fakeTab{}, nil
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := pool.acquire(context.Background(), open); err != nil {
			t.Error(err)
		}
	}()

	// the first query is inside open, waiting for its verdict
	<-opening

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := pool.acquire(ctx, open); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v: a second query must wait while the only place is being opened", err)
	}

	close(finish)
	<-done

	if n := atomic.LoadInt32(&opened); n != 1 {
		t.Errorf("opened %d tabs, want 1", n)
	}
}

// TestSetDefaultParamsKeepsLazyAttach guards the way from opening a tab to loading its page:
// without LazyAttach the page would silently take the r.8 branch after a lazy attach.
func TestSetDefaultParamsKeepsLazyAttach(t *testing.T) {
	drv := NewDriver()
	opts := lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})

	if got := drv.setDefaultParams(lazyParams(opts)).LazyAttach; got != opts {
		t.Errorf("got %+v, want the same lazyAttach", got)
	}
}

// TestCloseFailedPageLogsAtTrace: the message is frequent in production logs, so it is written
// at trace level, which ferret does not print at log level 0 (debug in zerolog).
func TestCloseFailedPageLogsAtTrace(t *testing.T) {
	fail := func() error { return errors.New("target closed") }

	for _, c := range []struct {
		level zerolog.Level
		want  bool
	}{{zerolog.DebugLevel, false}, {zerolog.TraceLevel, true}} {
		var buf bytes.Buffer

		closeFailedPage(zerolog.New(&buf).Level(c.level), errors.New("load failed"), fail, fail)

		if got := bytes.Contains(buf.Bytes(), []byte("failed to close browser page after a failed load")); got != c.want {
			t.Errorf("level %s: record %v, want %v (%q)", c.level, got, c.want, buf.String())
		}
	}
}

func TestCloseFailedPageStopsAtFirstSuccess(t *testing.T) {
	var buf bytes.Buffer
	targetCalls := 0

	closeFailedPage(zerolog.New(&buf).Level(zerolog.TraceLevel), errors.New("load failed"),
		func() error { return nil },
		func() error { targetCalls++; return nil },
	)

	if targetCalls != 0 || buf.Len() != 0 {
		t.Errorf("closing the target after a closed page: %d calls, log %q", targetCalls, buf.String())
	}
}
