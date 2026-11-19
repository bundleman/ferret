package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
)

// logReport writes the record of the report and returns it, checking that it is the only one.
func logReport(report *lazyAttachReport, err error) map[string]interface{} {
	var buf bytes.Buffer

	report.log(zerolog.New(&buf).Level(zerolog.DebugLevel), err)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	So(len(lines), ShouldEqual, 1)

	var rec map[string]interface{}

	So(json.Unmarshal(lines[0], &rec), ShouldBeNil)

	return rec
}

func Test_LazyAttachReport_Log(t *testing.T) {
	tests := []struct {
		name string
		// call fills a report as a call does and returns it with the error of the call
		call func() (*lazyAttachReport, error)
		want map[string]interface{}
	}{
		{
			name: "Should write every field of a call that succeeded",
			call: func() (*lazyAttachReport, error) {
				report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
				report.setIncognito(false)
				report.setTarget("T1")
				report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*", URL: waitTestURL, Title: "Товар 1", Wait: 4730 * time.Millisecond})
				report.setCommitted(true)
				report.setParsed("ready", 150*time.Millisecond)

				return report, nil
			},
			want: map[string]interface{}{
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
			},
		},
		{
			name: "Should write a skipped wait for parsing",
			call: func() (*lazyAttachReport, error) {
				report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
				report.setVerdict(lazyVerdict{Reason: verdictURL, Pattern: "*/captcha/*"})
				report.setCommitted(true)
				report.setParsed("skipped", 0)

				return report, nil
			},
			want: map[string]interface{}{"verdict": "url", "parsed": "skipped", "result": "ok"},
		},
		{
			name: "Should mark the steps not reached by a call that failed before the first poll",
			call: func() (*lazyAttachReport, error) {
				report := newLazyAttachReport(drivers.Params{URL: waitTestURL})
				cause := errors.New("failed to initialize driver")
				report.fail(stagePrepare, cause)

				return report, cause
			},
			want: map[string]interface{}{
				"verdict":   "before_poll",
				"committed": "not_reached",
				"parsed":    "not_reached",
				"result":    "error",
				"stage":     "prepare",
				"error":     "failed to initialize driver",
			},
		},
		{
			name: "Should write a poll error with the text kept",
			call: func() (*lazyAttachReport, error) {
				opener, _, _ := newTestOpener(&scriptedTab{err: rpcc.ErrConnClosing})
				report := newLazyAttachReport(lazyParams(nil))

				_, err := opener.open(context.Background(), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

				return report, err
			},
			want: map[string]interface{}{
				"target_id": "T1",
				"incognito": true,
				"verdict":   "poll_error",
				"committed": "not_reached",
				"parsed":    "not_reached",
				"result":    "error",
				"stage":     "poll",
				"error":     rpcc.ErrConnClosing.Error(),
			},
		},
		{
			name: "Should write an error the steps did not mark",
			call: func() (*lazyAttachReport, error) {
				return newLazyAttachReport(drivers.Params{URL: waitTestURL}), errors.New("boom")
			},
			want: map[string]interface{}{"result": "error", "stage": "unknown", "error": "boom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				rec := logReport(tt.call())
				got := make(map[string]interface{}, len(tt.want))

				for k := range tt.want {
					got[k] = rec[k]
				}

				So(got, ShouldResemble, tt.want)
			})
		})
	}
}

func Test_LazyAttachReport_LogWithoutReport(t *testing.T) {
	Convey("Should not write a call without lazyAttach", t, func() {
		var buf bytes.Buffer
		var report *lazyAttachReport

		report.log(zerolog.New(&buf), errors.New("boom"))

		So(buf.Len(), ShouldEqual, 0)
	})
}

// A new fetchMode tab with lazyAttach waits for its verdict inside the open callback.
func Test_FetchPool_HoldsSlotWhileOpening(t *testing.T) {
	Convey("Should keep the place of a tab being opened", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
		var opened int32
		opening := make(chan struct{})
		finish := make(chan struct{})
		release := sync.OnceFunc(func() { close(finish) })
		t.Cleanup(release)

		open := func(ctx context.Context) (fetchTab, error) {
			if atomic.AddInt32(&opened, 1) == 1 {
				close(opening)
			}

			// A second query let into open must not hang the test until its timeout.
			select {
			case <-finish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			return &fakeTab{}, nil
		}

		done := make(chan error, 1)

		go func() {
			_, err := pool.acquire(context.Background(), open)
			done <- err
		}()

		// the first query is inside open, waiting for its verdict
		select {
		case <-opening:
		case <-time.After(time.Second):
			t.Fatal("the first query did not reach open")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		t.Cleanup(cancel)

		_, err := pool.acquire(ctx, open)
		So(errors.Is(err, context.DeadlineExceeded), ShouldBeTrue)

		release()

		select {
		case err := <-done:
			So(err, ShouldBeNil)
		case <-time.After(time.Second):
			t.Fatal("the first query did not finish")
		}

		So(atomic.LoadInt32(&opened), ShouldEqual, 1)
	})
}

// Without LazyAttach the page would silently take the r.8 branch after a lazy attach.
func Test_Driver_SetDefaultParams(t *testing.T) {
	Convey("Should keep lazyAttach", t, func() {
		drv := NewDriver()
		opts := lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})

		So(drv.setDefaultParams(lazyParams(opts)).LazyAttach, ShouldPointTo, opts)
	})
}

// The message is frequent in production logs, so it is written at trace level, which ferret does
// not print at log level 0 (debug in zerolog).
func Test_CloseFailedPage_LogLevel(t *testing.T) {
	tests := []struct {
		name  string
		level zerolog.Level
		want  bool
	}{
		{name: "Should not write the record at debug level", level: zerolog.DebugLevel, want: false},
		{name: "Should write the record at trace level", level: zerolog.TraceLevel, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var buf bytes.Buffer

				fail := func() error { return errors.New("target closed") }

				closeFailedPage(zerolog.New(&buf).Level(tt.level), errors.New("load failed"), fail, fail)

				So(bytes.Contains(buf.Bytes(), []byte("failed to close browser page after a failed load")), ShouldEqual, tt.want)
			})
		})
	}
}

func Test_CloseFailedPage_StopsAtFirstSuccess(t *testing.T) {
	Convey("Should not close the target after the page was closed", t, func() {
		var buf bytes.Buffer
		targetCalls := 0

		closeFailedPage(zerolog.New(&buf).Level(zerolog.TraceLevel), errors.New("load failed"),
			func() error { return nil },
			func() error { targetCalls++; return nil },
		)

		So(targetCalls, ShouldEqual, 0)
		So(buf.Len(), ShouldEqual, 0)
	})
}
