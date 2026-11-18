package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
)

// openPageCtx returns a context whose ferret log goes to buf at level 0, as in production.
func openPageCtx(buf *bytes.Buffer) context.Context {
	return logging.WithContext(context.Background(), logging.Options{Writer: buf, Level: logging.Level(0)})
}

// lazyRecords returns the "lazyAttach document" records of the log.
func lazyRecords(t *testing.T, buf *bytes.Buffer) []map[string]interface{} {
	t.Helper()

	var res []map[string]interface{}

	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var rec map[string]interface{}

		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}

		if rec["message"] == "lazyAttach document" {
			res = append(res, rec)
		}
	}

	return res
}

func TestOpenPageOneRecordOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	var steps []string

	connect := func(_ context.Context, _ drivers.Params, report *lazyAttachReport) (*rpcc.Conn, error) {
		steps = append(steps, "connect")
		report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*"})

		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, params drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
		steps = append(steps, "load")
		report.setCommitted(true)
		report.setParsed("ready", time.Millisecond)

		// headers, userAgent and viewport reach the page session only here, after the attach
		if params.Headers == nil || params.UserAgent != "UA" || params.Viewport == nil {
			t.Errorf("the load step must get the session params, got %+v", params)
		}

		return &HTMLPage{}, nil
	}

	params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	params.Headers = drivers.NewHTTPHeadersWith(map[string][]string{"X-Test": {"1"}})
	params.UserAgent = "UA"

	page, err := NewDriver().openPageWith(openPageCtx(&buf), params, true, connect, load)

	if err != nil || page == nil {
		t.Fatalf("got %v, %v", page, err)
	}

	if len(steps) != 2 || steps[0] != "connect" || steps[1] != "load" {
		t.Errorf("steps %v, want the load after the attach", steps)
	}

	recs := lazyRecords(t, &buf)

	if len(recs) != 1 || recs[0]["result"] != "ok" || recs[0]["verdict"] != "title" || recs[0]["parsed"] != "ready" {
		t.Errorf("want one ok record, got %v", recs)
	}
}

// TestOpenPageRecordOnConnectError: a call that failed before the first poll is written with
// the incognito flag it would have had, not the zero value.
func TestOpenPageRecordOnConnectError(t *testing.T) {
	var buf bytes.Buffer

	connect := func(_ context.Context, _ drivers.Params, report *lazyAttachReport) (*rpcc.Conn, error) {
		err := errors.New("failed to initialize driver")
		report.fail(stagePrepare, err)

		return nil, err
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		t.Error("no load after a failed connection")

		return nil, nil
	}

	page, err := NewDriver().openPageWith(openPageCtx(&buf), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), true, connect, load)

	if err == nil || page != nil {
		t.Fatalf("got %v, %v", page, err)
	}

	recs := lazyRecords(t, &buf)

	if len(recs) != 1 || recs[0]["verdict"] != "before_poll" || recs[0]["incognito"] != true ||
		recs[0]["result"] != "error" || recs[0]["stage"] != "prepare" || recs[0]["committed"] != "not_reached" {
		t.Errorf("want one before_poll record of an incognito call, got %v", recs)
	}
}

func TestOpenPageRecordOnLoadError(t *testing.T) {
	var buf bytes.Buffer
	failed := &HTMLPage{}

	connect := func(_ context.Context, _ drivers.Params, report *lazyAttachReport) (*rpcc.Conn, error) {
		report.setVerdict(lazyVerdict{Reason: verdictTimeout})

		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
		err := context.DeadlineExceeded
		report.setCommitted(false)
		report.fail(stageFirstNavigation, err)

		return failed, err
	}

	params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	params.KeepCookies = true

	page, err := NewDriver().openPageWith(openPageCtx(&buf), params, true, connect, load)

	if !errors.Is(err, context.DeadlineExceeded) || page != failed {
		t.Fatalf("got %v, %v: a failed load returns its page and error as before", page, err)
	}

	recs := lazyRecords(t, &buf)

	if len(recs) != 1 || recs[0]["result"] != "error" || recs[0]["stage"] != "first_navigation" ||
		recs[0]["incognito"] != false || recs[0]["committed"] != "no" {
		t.Errorf("want one error record of a keepCookies call, got %v", recs)
	}
}

func TestOpenPageWithoutLazyAttachWritesNothing(t *testing.T) {
	var buf bytes.Buffer

	connect := func(_ context.Context, _ drivers.Params, report *lazyAttachReport) (*rpcc.Conn, error) {
		if report != nil {
			t.Error("a call without lazyAttach has no report")
		}

		return &rpcc.Conn{}, nil
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		return &HTMLPage{}, nil
	}

	if _, err := NewDriver().openPageWith(openPageCtx(&buf), drivers.Params{URL: waitTestURL, DirectNavigation: true}, true, connect, load); err != nil {
		t.Fatal(err)
	}

	if recs := lazyRecords(t, &buf); len(recs) != 0 {
		t.Errorf("want no record, got %v", recs)
	}
}

// TestOpenFetchPassesLazyAttachToNewTab: the pool opens a new tab through openPage with the same
// params, so lazyAttach acts on it; a reused tab is handed out by the pool without the open step
// (TestFetchPool), so it gets no lazyAttach and writes no record.
func TestOpenFetchPassesLazyAttachToNewTab(t *testing.T) {
	var buf bytes.Buffer
	drv := NewDriver()
	defer drv.Close()

	opts := lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})
	params := lazyParams(opts)
	params.Fetch = &drivers.FetchParams{PoolKey: "k"}
	calls := 0

	open := func(_ context.Context, got drivers.Params, logConnErr bool) (*HTMLPage, error) {
		calls++

		if got.LazyAttach != opts || logConnErr {
			t.Errorf("the new tab must be opened with the same lazyAttach and without the Open error log, got %+v, %v", got, logConnErr)
		}

		return &HTMLPage{}, nil
	}

	page, err := drv.openFetchWith(openPageCtx(&buf), params, open)

	if err != nil || page == nil || calls != 1 {
		t.Errorf("got %v, %v after %d opens", page, err, calls)
	}
}

// TestOpenPageConnErrorLog: Open logs a failed connection as before, a new fetchMode tab does not.
func TestOpenPageConnErrorLog(t *testing.T) {
	connect := func(_ context.Context, _ drivers.Params, _ *lazyAttachReport) (*rpcc.Conn, error) {
		return nil, errors.New("failed to initialize driver")
	}
	load := func(_ context.Context, _ *rpcc.Conn, _ drivers.Params, _ *lazyAttachReport) (*HTMLPage, error) {
		return nil, nil
	}

	for _, logConnErr := range []bool{true, false} {
		var buf bytes.Buffer

		_, _ = NewDriver().openPageWith(openPageCtx(&buf), drivers.Params{URL: waitTestURL}, logConnErr, connect, load)

		if got := bytes.Contains(buf.Bytes(), []byte("failed to create a new connection")); got != logConnErr {
			t.Errorf("logConnErr %v: record %v", logConnErr, got)
		}
	}
}
