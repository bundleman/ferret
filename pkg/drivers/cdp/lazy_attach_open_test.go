package cdp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp/protocol/storage"
	"github.com/mafredri/cdp/protocol/target"
	"github.com/mafredri/cdp/rpcc"

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

func TestOpenTargetDialsOnlyAfterVerdict(t *testing.T) {
	placeholder := tabState{waitTestURL, "shop.example/product/1/", false}
	opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{placeholder, placeholder, {waitTestURL, "Товар 1", false}}})
	report := newLazyAttachReport(lazyParams(nil))

	conn, err := opener.open(context.Background(), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

	if err != nil || conn == nil {
		t.Fatalf("got %v, %v", conn, err)
	}

	if dial.calls != 1 || dial.pollsAtCall != 3 {
		t.Errorf("dial called %d times after %d polls, want once after the verdict poll 3", dial.calls, dial.pollsAtCall)
	}

	if report.verdict.Reason != verdictTitle || report.targetID != "T1" || !report.incognito {
		t.Errorf("got report %+v", report)
	}

	if len(targets.created) != 1 || targets.created[0].URL != waitTestURL {
		t.Errorf("the tab must be created on the target URL, got %+v", targets.created)
	}
}

func TestOpenTargetPollErrorClosesTab(t *testing.T) {
	opener, targets, dial := newTestOpener(&scriptedTab{err: rpcc.ErrConnClosing})
	report := newLazyAttachReport(lazyParams(nil))

	_, err := opener.open(context.Background(), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

	// the downloader closes the driver by this text
	if err == nil || !strings.Contains(err.Error(), "the connection is closing") || !errors.Is(err, rpcc.ErrConnClosing) {
		t.Errorf("got %v, want the poll error with its text kept", err)
	}

	if dial.calls != 0 || len(targets.closed) != 1 || targets.closed[0] != "T1" {
		t.Errorf("dial calls %d, closed %v: want the tab closed and never attached", dial.calls, targets.closed)
	}

	if report.verdict.Reason != verdictPollError || report.stage != stagePoll {
		t.Errorf("got report %+v", report)
	}
}

func TestOpenTargetContextEndClosesTab(t *testing.T) {
	opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "", false}}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	report := newLazyAttachReport(lazyParams(nil))

	_, err := opener.open(ctx, lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the context error", err)
	}

	if dial.calls != 0 || len(targets.closed) != 1 || !targets.closedOnAlive[0] {
		t.Errorf("dial calls %d, closed %v on a live context %v: want the tab closed on a context of its own",
			dial.calls, targets.closed, targets.closedOnAlive)
	}
}

func TestOpenTargetDialErrorClosesTab(t *testing.T) {
	opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}})
	dial.err = rpcc.ErrConnClosing
	report := newLazyAttachReport(lazyParams(nil))

	_, err := opener.open(context.Background(), lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"})), report)

	if !errors.Is(err, rpcc.ErrConnClosing) || len(targets.closed) != 1 {
		t.Errorf("got %v, closed %v: want the error and the tab closed", err, targets.closed)
	}

	if report.stage != stageAttach {
		t.Errorf("got stage %q, want %q", report.stage, stageAttach)
	}
}

func TestOpenTargetTimeoutStillAttaches(t *testing.T) {
	opener, _, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "", false}}})
	report := newLazyAttachReport(lazyParams(nil))

	_, err := opener.open(context.Background(), lazyParams(lazyOpts(60*time.Millisecond, 10*time.Millisecond, nil, []string{"*"})), report)

	if err != nil || dial.calls != 1 || report.verdict.Reason != verdictTimeout {
		t.Errorf("got %v, dial calls %d, report %+v: the end of timeout attaches as usual", err, dial.calls, report)
	}
}

func TestOpenTargetWithoutLazyAttachUnchanged(t *testing.T) {
	opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "", false}}})

	conn, err := opener.open(context.Background(), drivers.Params{URL: waitTestURL, DirectNavigation: true}, nil)

	if err != nil || conn == nil || dial.calls != 1 || targets.infoCount() != 0 {
		t.Errorf("got %v, dial calls %d, polls %d: want an attach right away and no polls", err, dial.calls, targets.infoCount())
	}

	if len(targets.created) != 1 || targets.created[0].URL != waitTestURL {
		t.Errorf("got %+v, want the tab created on the target URL", targets.created)
	}
}

func TestOpenTargetWithoutDirectNavigationOpensBlank(t *testing.T) {
	opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "", false}}})

	_, err := opener.open(context.Background(), drivers.Params{URL: waitTestURL}, nil)

	if err != nil || dial.calls != 1 || len(targets.created) != 1 || targets.created[0].URL != BlankPageURL {
		t.Errorf("got %v, dial calls %d, created %+v: want a blank tab as before", err, dial.calls, targets.created)
	}
}

func TestOpenTargetKeepCookiesIsNotIncognito(t *testing.T) {
	opener, targets, _ := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}})
	params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
	params.KeepCookies = true
	report := newLazyAttachReport(params)

	if _, err := opener.open(context.Background(), params, report); err != nil {
		t.Fatal(err)
	}

	if report.incognito || targets.created[0].BrowserContextID != nil {
		t.Errorf("a keepCookies tab lives in the default context, got report %+v, args %+v", report, targets.created[0])
	}
}

// TestOpenTargetPrepareErrors: a failure while setting cookies or creating the tab is written as
// a call that failed before the first poll, with the stage prepare.
func TestOpenTargetPrepareErrors(t *testing.T) {
	for _, name := range []string{"cookies", "create target"} {
		opener, targets, dial := newTestOpener(&scriptedTab{states: []tabState{{waitTestURL, "Товар 1", false}}})
		params := lazyParams(lazyOpts(time.Second, 10*time.Millisecond, nil, []string{"*"}))
		params.Cookies = drivers.NewHTTPCookiesWith(map[string]drivers.HTTPCookie{"a": {Name: "a", Value: "1"}})

		if name == "cookies" {
			opener.cookies = &fakeCookies{err: errors.New("cookies failed")}
		} else {
			targets.createErr = errors.New("create failed")
		}

		report := newLazyAttachReport(params)
		_, err := opener.open(context.Background(), params, report)

		if err == nil || report.stage != stagePrepare || report.verdict.Reason != verdictBeforePoll {
			t.Errorf("%s: got %v, report %+v", name, err, report)
		}

		if dial.calls != 0 || targets.infoCount() != 0 || len(targets.closed) != 0 {
			t.Errorf("%s: dial %d, polls %d, closed %v: nothing to attach to or close", name, dial.calls, targets.infoCount(), targets.closed)
		}
	}
}
