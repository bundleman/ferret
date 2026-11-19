package cdp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp/protocol/storage"
	"github.com/mafredri/cdp/protocol/target"
	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
)

// fakeTargets is the browser side of opening a tab: it creates one tab, answers its polls from
// a scriptedTab and records what was closed and on which context.
type fakeTargets struct {
	mu            sync.Mutex
	tab           *scriptedTab
	createErr     error
	created       []*target.CreateTargetArgs
	infoCalls     int
	closed        []target.ID
	closedOnAlive []bool
}

func (f *fakeTargets) CreateTarget(_ context.Context, args *target.CreateTargetArgs) (*target.CreateTargetReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.created = append(f.created, args)

	if f.createErr != nil {
		return nil, f.createErr
	}

	return &target.CreateTargetReply{TargetID: "T1"}, nil
}

func (f *fakeTargets) GetTargetInfo(ctx context.Context, args *target.GetTargetInfoArgs) (*target.GetTargetInfoReply, error) {
	f.mu.Lock()
	f.infoCalls++
	f.mu.Unlock()

	if args.TargetID == nil || *args.TargetID != "T1" {
		return nil, errors.New("the poll must name the tab")
	}

	u, title, attached, err := f.tab.poll(ctx)

	if err != nil {
		return nil, err
	}

	return &target.GetTargetInfoReply{TargetInfo: target.Info{TargetID: "T1", URL: u, Title: title, Attached: attached}}, nil
}

func (f *fakeTargets) CloseTarget(ctx context.Context, args *target.CloseTargetArgs) (*target.CloseTargetReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = append(f.closed, args.TargetID)
	f.closedOnAlive = append(f.closedOnAlive, ctx.Err() == nil)

	return &target.CloseTargetReply{}, nil
}

func (f *fakeTargets) infoCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.infoCalls
}

type fakeCookies struct {
	set []*storage.SetCookiesArgs
	err error
}

func (f *fakeCookies) SetCookies(_ context.Context, args *storage.SetCookiesArgs) error {
	f.set = append(f.set, args)

	return f.err
}

// fakeDial records when the tab was attached: how many polls had been made by then.
type fakeDial struct {
	targets     *fakeTargets
	err         error
	calls       int
	pollsAtCall int
}

func (d *fakeDial) dial(_ context.Context, id target.ID) (*rpcc.Conn, error) {
	d.calls++
	d.pollsAtCall = d.targets.infoCount()

	if id != "T1" {
		return nil, errors.New("dial must attach to the created tab")
	}

	if d.err != nil {
		return nil, d.err
	}

	return &rpcc.Conn{}, nil
}

func newTestOpener(tab *scriptedTab) (targetOpener, *fakeTargets, *fakeDial) {
	targets := &fakeTargets{tab: tab}
	dial := &fakeDial{targets: targets}

	return targetOpener{targets: targets, cookies: &fakeCookies{}, dial: dial.dial, contextID: "CTX"}, targets, dial
}

func lazyParams(opts *drivers.LazyAttach) drivers.Params {
	return drivers.Params{URL: waitTestURL, DirectNavigation: true, LazyAttach: opts}
}

func Test_TargetOpener_Open(t *testing.T) {
	type fields struct {
		tab *scriptedTab
		// setup breaks a part of the browser side before the call
		setup func(opener *targetOpener, targets *fakeTargets, dial *fakeDial)
	}
	type args struct {
		params   drivers.Params
		report   *lazyAttachReport
		deadline time.Duration
	}
	placeholder := tabState{waitTestURL, "shop.example/product/1/", false}
	verdictTab := func() *scriptedTab { return &scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}} }
	blankTab := func() *scriptedTab { return &scriptedTab{states: []tabState{{waitTestURL, "", false}}} }
	byTitle := func() drivers.Params {
		return lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	}
	keepCookies := byTitle()
	keepCookies.KeepCookies = true
	withCookies := func() drivers.Params {
		params := byTitle()
		params.Cookies = drivers.NewHTTPCookiesWith(map[string]drivers.HTTPCookie{"a": {Name: "a", Value: "1"}})

		return params
	}
	errCookies := errors.New("cookies failed")
	errCreate := errors.New("create failed")
	cookiesParams, createParams := withCookies(), withCookies()
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr error
		// errText is the text the downloader closes the driver by
		errText string
		assert  func(conn *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, dial *fakeDial)
	}{
		{
			name:   "Should attach only after the verdict",
			fields: fields{tab: &scriptedTab{states: []tabState{placeholder, placeholder, {waitTestURL, "Товар 1", false}}}},
			args:   args{params: byTitle(), report: newLazyAttachReport(lazyParams(nil))},
			assert: func(conn *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
				So(conn, ShouldNotBeNil)
				So(dial.calls, ShouldEqual, 1)
				So(dial.pollsAtCall, ShouldEqual, 3)
				So(report.verdict.Reason, ShouldEqual, verdictTitle)
				So(report.targetID, ShouldEqual, "T1")
				So(report.incognito, ShouldBeTrue)
				So(len(targets.created), ShouldEqual, 1)
				So(targets.created[0].URL, ShouldEqual, waitTestURL)
			},
		},
		{
			name:   "Should attach as usual at the end of timeout",
			fields: fields{tab: blankTab()},
			args: args{
				params: lazyParams(lazyOpts(60*time.Millisecond, 10*time.Millisecond, nil, []string{"*"})),
				report: newLazyAttachReport(lazyParams(nil)),
			},
			assert: func(_ *rpcc.Conn, report *lazyAttachReport, _ *fakeTargets, dial *fakeDial) {
				So(dial.calls, ShouldEqual, 1)
				So(report.verdict.Reason, ShouldEqual, verdictTimeout)
			},
		},
		{
			name:   "Should attach right away without lazyAttach",
			fields: fields{tab: blankTab()},
			args:   args{params: drivers.Params{URL: waitTestURL, DirectNavigation: true}},
			assert: func(conn *rpcc.Conn, _ *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
				So(conn, ShouldNotBeNil)
				So(dial.calls, ShouldEqual, 1)
				So(targets.infoCount(), ShouldEqual, 0)
				So(len(targets.created), ShouldEqual, 1)
				So(targets.created[0].URL, ShouldEqual, waitTestURL)
			},
		},
		{
			name:   "Should open a blank tab without directNavigation",
			fields: fields{tab: blankTab()},
			args:   args{params: drivers.Params{URL: waitTestURL}},
			assert: func(_ *rpcc.Conn, _ *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
				So(dial.calls, ShouldEqual, 1)
				So(len(targets.created), ShouldEqual, 1)
				So(targets.created[0].URL, ShouldEqual, BlankPageURL)
			},
		},
		{
			name:   "Should open a keepCookies tab in the default context",
			fields: fields{tab: verdictTab()},
			args:   args{params: keepCookies, report: newLazyAttachReport(keepCookies)},
			assert: func(_ *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, _ *fakeDial) {
				So(report.incognito, ShouldBeFalse)
				So(targets.created[0].BrowserContextID, ShouldBeNil)
			},
		},
		{
			name:    "Should close the tab on a poll error and keep the error text",
			fields:  fields{tab: &scriptedTab{err: rpcc.ErrConnClosing}},
			args:    args{params: byTitle(), report: newLazyAttachReport(lazyParams(nil))},
			wantErr: rpcc.ErrConnClosing,
			errText: "the connection is closing",
			assert: func(_ *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
				So(dial.calls, ShouldEqual, 0)
				So(len(targets.closed), ShouldEqual, 1)
				So(targets.closed[0], ShouldEqual, "T1")
				So(report.verdict.Reason, ShouldEqual, verdictPollError)
				So(report.stage, ShouldEqual, stagePoll)
			},
		},
		{
			name:    "Should close the tab on a context of its own when the call ends",
			fields:  fields{tab: blankTab()},
			args:    args{params: byTitle(), report: newLazyAttachReport(lazyParams(nil)), deadline: 50 * time.Millisecond},
			wantErr: context.DeadlineExceeded,
			assert: func(_ *rpcc.Conn, _ *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
				So(dial.calls, ShouldEqual, 0)
				So(len(targets.closed), ShouldEqual, 1)
				So(targets.closedOnAlive[0], ShouldBeTrue)
			},
		},
		{
			name: "Should close the tab when the attach fails",
			fields: fields{
				tab: verdictTab(),
				setup: func(_ *targetOpener, _ *fakeTargets, dial *fakeDial) {
					dial.err = rpcc.ErrConnClosing
				},
			},
			args:    args{params: byTitle(), report: newLazyAttachReport(lazyParams(nil))},
			wantErr: rpcc.ErrConnClosing,
			assert: func(_ *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, _ *fakeDial) {
				So(len(targets.closed), ShouldEqual, 1)
				So(report.stage, ShouldEqual, stageAttach)
			},
		},
		{
			name: "Should write a failure to set cookies as a failure before the first poll",
			fields: fields{
				tab: verdictTab(),
				setup: func(opener *targetOpener, _ *fakeTargets, _ *fakeDial) {
					opener.cookies = &fakeCookies{err: errCookies}
				},
			},
			args:    args{params: cookiesParams, report: newLazyAttachReport(cookiesParams)},
			wantErr: errCookies,
			assert:  assertPrepareFailure,
		},
		{
			name: "Should write a failure to create the tab as a failure before the first poll",
			fields: fields{
				tab: verdictTab(),
				setup: func(_ *targetOpener, targets *fakeTargets, _ *fakeDial) {
					targets.createErr = errCreate
				},
			},
			args:    args{params: createParams, report: newLazyAttachReport(createParams)},
			wantErr: errCreate,
			assert:  assertPrepareFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Convey(tt.name, t, func() {
				opener, targets, dial := newTestOpener(tt.fields.tab)

				if tt.fields.setup != nil {
					tt.fields.setup(&opener, targets, dial)
				}

				ctx := context.Background()

				if tt.args.deadline > 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, tt.args.deadline)
					t.Cleanup(cancel)
				}

				conn, err := opener.open(ctx, tt.args.params, tt.args.report)

				if tt.wantErr != nil {
					So(errors.Is(err, tt.wantErr), ShouldBeTrue)

					if tt.errText != "" {
						So(err.Error(), ShouldContainSubstring, tt.errText)
					}
				} else {
					So(err, ShouldBeNil)
				}

				tt.assert(conn, tt.args.report, targets, dial)
			})
		})
	}
}

// assertPrepareFailure checks a call that failed before the first poll: nothing to attach to or close.
func assertPrepareFailure(_ *rpcc.Conn, report *lazyAttachReport, targets *fakeTargets, dial *fakeDial) {
	So(report.stage, ShouldEqual, stagePrepare)
	So(report.verdict.Reason, ShouldEqual, verdictBeforePoll)
	So(dial.calls, ShouldEqual, 0)
	So(targets.infoCount(), ShouldEqual, 0)
	So(len(targets.closed), ShouldEqual, 0)
}
