package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mafredri/cdp/protocol/browser"
	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
)

// openPageCtx returns a context whose ferret log goes to buf at level 0, as in production.
func openPageCtx(buf *bytes.Buffer) context.Context {
	return logging.WithContext(context.Background(), logging.Options{Writer: buf, Level: logging.Level(0)})
}

// lazyRecords returns the "lazyAttach document" records of the log.
func lazyRecords(buf *bytes.Buffer) []map[string]interface{} {
	var res []map[string]interface{}

	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var rec map[string]interface{}

		So(json.Unmarshal(line, &rec), ShouldBeNil)

		if rec["message"] == "lazyAttach document" {
			res = append(res, rec)
		}
	}

	return res
}

// openPageFakes plays the connect and load steps of openPageWith and records them next to the
// isolate step of iso, so the order of all steps can be checked.
func openPageFakes(
	iso *isolation,
	connect func(report *lazyAttachReport) (*rpcc.Conn, error),
	load func(params drivers.Params, report *lazyAttachReport) (*HTMLPage, error),
) (connectFunc, loadFunc) {
	return func(_ context.Context, _ drivers.Params, contextID browser.ContextID, report *lazyAttachReport) (*rpcc.Conn, error) {
			iso.step("connect:" + string(contextID))

			return connect(report)
		}, func(_ context.Context, _ *rpcc.Conn, params drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
			iso.step("load")

			return load(params, report)
		}
}

func Test_Driver_OpenPageWith_LazyAttachRecord(t *testing.T) {
	type fields struct {
		connect func(report *lazyAttachReport) (*rpcc.Conn, error)
		load    func(params drivers.Params, report *lazyAttachReport) (*HTMLPage, error)
	}
	type args struct {
		params     drivers.Params
		logConnErr bool
	}
	withSession := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	withSession.Headers = drivers.NewHTTPHeadersWith(map[string][]string{"X-Test": {"1"}})
	withSession.UserAgent = "UA"
	keepCookies := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	keepCookies.KeepCookies = true
	errInit := errors.New("failed to initialize driver")
	failed := &HTMLPage{}
	noPage := func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return nil, nil }
	failInit := func(*lazyAttachReport) (*rpcc.Conn, error) { return nil, errInit }
	// what the fakes were given, checked after the call
	var loaded drivers.Params
	var connectReport *lazyAttachReport
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr error
		assert  func(page *HTMLPage, iso *isolation, log *bytes.Buffer)
	}{
		{
			name: "Should write one ok record after the load",
			fields: fields{
				connect: func(report *lazyAttachReport) (*rpcc.Conn, error) {
					report.setVerdict(lazyVerdict{Reason: verdictTitle, Pattern: "*"})

					return &rpcc.Conn{}, nil
				},
				load: func(params drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
					loaded = params
					report.setCommitted(true)
					report.setParsed("ready", time.Millisecond)

					return &HTMLPage{}, nil
				},
			},
			args: args{params: withSession, logConnErr: true},
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page, ShouldNotBeNil)
				So(iso.steps, ShouldResemble, []string{"connect:", "load"})
				// headers, userAgent and viewport reach the page session only at the load, after the attach
				So(loaded.Headers, ShouldNotBeNil)
				So(loaded.UserAgent, ShouldEqual, "UA")
				So(loaded.Viewport, ShouldNotBeNil)

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["result"], ShouldEqual, "ok")
				So(recs[0]["verdict"], ShouldEqual, "title")
				So(recs[0]["parsed"], ShouldEqual, "ready")
			},
		},
		{
			name: "Should write nothing without lazyAttach",
			fields: fields{
				connect: func(report *lazyAttachReport) (*rpcc.Conn, error) {
					connectReport = report

					return &rpcc.Conn{}, nil
				},
				load: func(drivers.Params, *lazyAttachReport) (*HTMLPage, error) { return &HTMLPage{}, nil },
			},
			args: args{params: drivers.Params{URL: waitTestURL, DirectNavigation: true}, logConnErr: true},
			assert: func(_ *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(iso.steps, ShouldResemble, []string{"connect:", "load"})
				So(connectReport, ShouldBeNil)
				So(len(lazyRecords(log)), ShouldEqual, 0)
			},
		},
		{
			// The incognito flag of the call is known before the driver is initialized.
			name: "Should write a call that failed before the first poll with its incognito flag",
			fields: fields{
				connect: func(report *lazyAttachReport) (*rpcc.Conn, error) {
					report.fail(stagePrepare, errInit)

					return nil, errInit
				},
				load: noPage,
			},
			args:    args{params: lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), logConnErr: true},
			wantErr: errInit,
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page, ShouldBeNil)
				So(iso.steps, ShouldResemble, []string{"connect:"})

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["verdict"], ShouldEqual, "before_poll")
				So(recs[0]["incognito"], ShouldEqual, true)
				So(recs[0]["result"], ShouldEqual, "error")
				So(recs[0]["stage"], ShouldEqual, "prepare")
				So(recs[0]["committed"], ShouldEqual, "not_reached")
			},
		},
		{
			name: "Should return the page of a failed load with its error and write an error record",
			fields: fields{
				connect: func(report *lazyAttachReport) (*rpcc.Conn, error) {
					report.setVerdict(lazyVerdict{Reason: verdictTimeout})

					return &rpcc.Conn{}, nil
				},
				load: func(_ drivers.Params, report *lazyAttachReport) (*HTMLPage, error) {
					report.setCommitted(false)
					report.fail(stageFirstNavigation, context.DeadlineExceeded)

					return failed, context.DeadlineExceeded
				},
			},
			args:    args{params: keepCookies, logConnErr: true},
			wantErr: context.DeadlineExceeded,
			assert: func(page *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(page, ShouldPointTo, failed)
				So(iso.steps, ShouldResemble, []string{"connect:", "load"})

				recs := lazyRecords(log)
				So(len(recs), ShouldEqual, 1)
				So(recs[0]["result"], ShouldEqual, "error")
				So(recs[0]["stage"], ShouldEqual, "first_navigation")
				So(recs[0]["incognito"], ShouldEqual, false)
				So(recs[0]["committed"], ShouldEqual, "no")
			},
		},
		{
			name:    "Should log a failed connection of Open",
			fields:  fields{connect: failInit, load: noPage},
			args:    args{params: drivers.Params{URL: waitTestURL}, logConnErr: true},
			wantErr: errInit,
			assert: func(_ *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(iso.steps, ShouldResemble, []string{"connect:"})
				So(log.String(), ShouldContainSubstring, "failed to create a new connection")
			},
		},
		{
			name:    "Should not log a failed connection of a new fetchMode tab",
			fields:  fields{connect: failInit, load: noPage},
			args:    args{params: drivers.Params{URL: waitTestURL}, logConnErr: false},
			wantErr: errInit,
			assert: func(_ *HTMLPage, iso *isolation, log *bytes.Buffer) {
				So(iso.steps, ShouldResemble, []string{"connect:"})
				So(log.String(), ShouldNotContainSubstring, "failed to create a new connection")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				var log bytes.Buffer

				iso := &isolation{}
				connect, load := openPageFakes(iso, tt.fields.connect, tt.fields.load)

				page, err := NewDriver().openPageWith(openPageCtx(&log), tt.args.params, tt.args.logConnErr, iso.isolate, connect, load)

				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)
				} else {
					So(err, ShouldBeNil)
				}

				tt.assert(page, iso, &log)
			})
		})
	}
}

// The pool opens a new tab through openPage with the same params, so lazyAttach acts on it; a
// reused tab is handed out by the pool without the open step (TestFetchPool), so it gets no
// lazyAttach and writes no record.
func Test_Driver_OpenFetchWith(t *testing.T) {
	Convey("Should open a new fetchMode tab with the same lazyAttach and without the Open error log", t, func() {
		var buf bytes.Buffer
		drv := NewDriver()
		t.Cleanup(func() { _ = drv.Close() })

		opts := lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})
		params := lazyParams(opts)
		params.Fetch = &drivers.FetchParams{PoolKey: "k"}
		calls := 0
		var gotLazy *drivers.LazyAttach
		gotLogConnErr := true

		open := func(_ context.Context, got drivers.Params, logConnErr bool) (*HTMLPage, error) {
			calls++
			gotLazy, gotLogConnErr = got.LazyAttach, logConnErr

			return &HTMLPage{}, nil
		}

		page, err := drv.openFetchWith(openPageCtx(&buf), params, open)

		So(err, ShouldBeNil)
		So(page, ShouldNotBeNil)
		So(calls, ShouldEqual, 1)
		So(gotLazy, ShouldPointTo, opts)
		So(gotLogConnErr, ShouldBeFalse)
	})
}
