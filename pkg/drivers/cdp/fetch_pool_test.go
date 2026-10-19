package cdp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
)

type fakeTab struct {
	mu        sync.Mutex
	pingErr   error
	pingDelay time.Duration
	destroyed int
}

func (t *fakeTab) ping(ctx context.Context) error {
	t.mu.Lock()
	delay, err := t.pingDelay, t.pingErr
	t.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return err
}

func (t *fakeTab) destroy() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.destroyed++

	return nil
}

func (t *fakeTab) setPingErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pingErr = err
}

func (t *fakeTab) destroyedTimes() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.destroyed
}

type fakeOpener struct {
	mu     sync.Mutex
	opened []*fakeTab
	err    error
}

func (o *fakeOpener) open(_ context.Context) (fetchTab, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.err != nil {
		return nil, o.err
	}

	tab := &fakeTab{}
	o.opened = append(o.opened, tab)

	return tab, nil
}

func (o *fakeOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return len(o.opened)
}

func TestFetchPool(t *testing.T) {
	ctx := context.Background()

	Convey("Should reuse a released tab instead of opening a new one", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		opener := &fakeOpener{}

		first, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(pool.release(first), ShouldBeTrue)

		second, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(second, ShouldEqual, first)
		So(opener.count(), ShouldEqual, 1)
	})

	Convey("Should fill defaults for omitted limits", t, func() {
		pool := newFetchPool(drivers.FetchParams{})

		So(pool.size, ShouldEqual, defaultFetchPoolSize)
		So(pool.maxUses, ShouldEqual, defaultFetchMaxUses)
		So(pool.maxAge, ShouldEqual, defaultFetchMaxAge)
	})

	Convey("Should make a query wait while every tab is in use", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
		opener := &fakeOpener{}

		first, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)

		got := make(chan fetchTab, 1)

		go func() {
			tab, _ := pool.acquire(ctx, opener.open)
			got <- tab
		}()

		select {
		case <-got:
			t.Fatal("the second query must wait for the busy tab")
		case <-time.After(50 * time.Millisecond):
		}

		So(pool.release(first), ShouldBeTrue)

		select {
		case tab := <-got:
			So(tab, ShouldEqual, first)
		case <-time.After(time.Second):
			t.Fatal("the second query did not get the released tab")
		}

		So(opener.count(), ShouldEqual, 1)
	})

	Convey("Should stop waiting when the context expires", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
		opener := &fakeOpener{}

		_, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)

		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()

		_, err = pool.acquire(waitCtx, opener.open)
		So(errors.Is(err, context.DeadlineExceeded), ShouldBeTrue)
		So(opener.count(), ShouldEqual, 1)
	})

	Convey("Should return false for a tab the pool does not know", t, func() {
		pool := newFetchPool(drivers.FetchParams{})

		So(pool.release(&fakeTab{}), ShouldBeFalse)
	})

	Convey("Should ignore a repeated release of an idle tab", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		opener := &fakeOpener{}

		tab, _ := pool.acquire(ctx, opener.open)
		So(pool.release(tab), ShouldBeTrue)
		So(pool.release(tab), ShouldBeTrue)
		So(tab.(*fakeTab).destroyedTimes(), ShouldEqual, 0)
	})

	Convey("Should replace a tab that stopped answering", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		opener := &fakeOpener{}

		first, _ := pool.acquire(ctx, opener.open)
		pool.release(first)
		first.(*fakeTab).setPingErr(errors.New("target closed"))

		second, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(second, ShouldNotEqual, first)
		So(first.(*fakeTab).destroyedTimes(), ShouldEqual, 1)
		So(opener.count(), ShouldEqual, 2)
	})

	Convey("Should retire a tab after MaxUses queries", t, func() {
		pool := newFetchPool(drivers.FetchParams{MaxUses: 2})
		opener := &fakeOpener{}

		first, _ := pool.acquire(ctx, opener.open)
		pool.release(first)
		again, _ := pool.acquire(ctx, opener.open)
		So(again, ShouldEqual, first)
		pool.release(again)
		So(first.(*fakeTab).destroyedTimes(), ShouldEqual, 1)

		fresh, _ := pool.acquire(ctx, opener.open)
		So(fresh, ShouldNotEqual, first)
		So(opener.count(), ShouldEqual, 2)
	})

	Convey("Should retire a tab older than MaxAge", t, func() {
		pool := newFetchPool(drivers.FetchParams{MaxAge: time.Minute})
		now := time.Now()
		pool.now = func() time.Time { return now }
		opener := &fakeOpener{}

		first, _ := pool.acquire(ctx, opener.open)
		pool.release(first)

		now = now.Add(2 * time.Minute)

		fresh, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(fresh, ShouldNotEqual, first)
		So(first.(*fakeTab).destroyedTimes(), ShouldEqual, 1)
	})

	Convey("Should free the slot when opening a tab fails", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
		opener := &fakeOpener{err: errors.New("create a browser target")}

		_, err := pool.acquire(ctx, opener.open)
		So(err, ShouldNotBeNil)

		opener.mu.Lock()
		opener.err = nil
		opener.mu.Unlock()

		tab, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(tab, ShouldNotBeNil)
	})

	Convey("Should destroy tabs on close and refuse new queries", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 2})
		opener := &fakeOpener{}

		idle, _ := pool.acquire(ctx, opener.open)
		busy, _ := pool.acquire(ctx, opener.open)
		pool.release(idle)

		pool.close()
		So(idle.(*fakeTab).destroyedTimes(), ShouldEqual, 1)
		So(busy.(*fakeTab).destroyedTimes(), ShouldEqual, 0)

		So(pool.release(busy), ShouldBeTrue)
		So(busy.(*fakeTab).destroyedTimes(), ShouldEqual, 1)

		_, err := pool.acquire(ctx, opener.open)
		So(errors.Is(err, errFetchPoolClosed), ShouldBeTrue)
	})

	Convey("Should never lease more tabs than the pool size", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 2})
		opener := &fakeOpener{}

		var inUse, maxInUse int32
		var wg sync.WaitGroup

		for g := 0; g < 20; g++ {
			wg.Add(1)

			go func() {
				defer wg.Done()

				for i := 0; i < 20; i++ {
					tab, err := pool.acquire(ctx, opener.open)

					if err != nil {
						t.Error(err)

						return
					}

					n := atomic.AddInt32(&inUse, 1)

					for {
						m := atomic.LoadInt32(&maxInUse)

						if n <= m || atomic.CompareAndSwapInt32(&maxInUse, m, n) {
							break
						}
					}

					atomic.AddInt32(&inUse, -1)
					pool.release(tab)
				}
			}()
		}

		wg.Wait()

		So(atomic.LoadInt32(&maxInUse), ShouldBeLessThanOrEqualTo, 2)
		So(opener.count(), ShouldBeLessThanOrEqualTo, 2)
	})
	Convey("Should put a tab back when the query gave up before getting it", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		opener := &fakeOpener{}

		tab, _ := pool.acquire(ctx, opener.open)
		pool.release(tab)

		gone, cancel := context.WithCancel(ctx)
		cancel()

		_, err := pool.acquire(gone, opener.open)
		So(errors.Is(err, context.Canceled), ShouldBeTrue)
		So(tab.(*fakeTab).destroyedTimes(), ShouldEqual, 0)

		again, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(again, ShouldEqual, tab)
		So(opener.count(), ShouldEqual, 1)
	})

	Convey("Should ping a tab on its own deadline, not on the query's", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		opener := &fakeOpener{}

		tab, _ := pool.acquire(ctx, opener.open)
		pool.release(tab)
		tab.(*fakeTab).mu.Lock()
		tab.(*fakeTab).pingDelay = 100 * time.Millisecond
		tab.(*fakeTab).mu.Unlock()

		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()

		again, err := pool.acquire(short, opener.open)
		So(err, ShouldBeNil)
		So(again, ShouldEqual, tab)
		So(tab.(*fakeTab).destroyedTimes(), ShouldEqual, 0)
	})

	Convey("Should take back a lease its query never returned", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 1})
		now := time.Now()
		pool.now = func() time.Time { return now }
		opener := &fakeOpener{}

		abandoned, _ := pool.acquire(ctx, opener.open)

		now = now.Add(defaultFetchMaxLease)

		fresh, err := pool.acquire(ctx, opener.open)
		So(err, ShouldBeNil)
		So(fresh, ShouldNotEqual, abandoned)
		So(abandoned.(*fakeTab).destroyedTimes(), ShouldEqual, 1)
		So(pool.release(abandoned), ShouldBeFalse)
	})

	Convey("Should sweep idle and abandoned tabs and report an unused pool", t, func() {
		pool := newFetchPool(drivers.FetchParams{PoolSize: 3})
		now := time.Now()
		pool.now = func() time.Time { return now }
		opener := &fakeOpener{}

		idle, _ := pool.acquire(ctx, opener.open)
		busy, _ := pool.acquire(ctx, opener.open)
		pool.release(idle)

		now = now.Add(time.Minute)
		gone, empty := pool.sweep()
		So(gone, ShouldBeEmpty)
		So(empty, ShouldBeFalse)

		// the idle tab is past maxIdle, the busy one past maxLease, and nobody has asked
		// the pool for a tab since: it is reported unused and refuses new queries
		now = now.Add(defaultFetchMaxIdle)
		gone, empty = pool.sweep()
		So(len(gone), ShouldEqual, 2)
		So(gone, ShouldContain, idle)
		So(gone, ShouldContain, busy)
		So(empty, ShouldBeTrue)

		_, err := pool.acquire(ctx, opener.open)
		So(errors.Is(err, errFetchPoolClosed), ShouldBeTrue)
	})

	Convey("Should keep an empty pool that was asked for recently", t, func() {
		pool := newFetchPool(drivers.FetchParams{})
		now := time.Now()
		pool.now = func() time.Time { return now }

		gone, empty := pool.sweep()
		So(gone, ShouldBeEmpty)
		So(empty, ShouldBeFalse)
	})

	Convey("Should keep keepCookies tabs apart from incognito tabs of the same key", t, func() {
		drv := NewDriver()
		defer drv.closeFetchPools()

		incognito, key := drv.fetchPool(drivers.FetchParams{PoolKey: "wb"}, false)
		kept, keptKey := drv.fetchPool(drivers.FetchParams{PoolKey: "wb"}, true)
		same, _ := drv.fetchPool(drivers.FetchParams{PoolKey: "wb"}, false)

		So(kept, ShouldNotEqual, incognito)
		So(same, ShouldEqual, incognito)
		So(key, ShouldEqual, "wb")
		So(keptKey, ShouldEqual, "wb")
	})
}
