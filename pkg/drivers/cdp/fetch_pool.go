package cdp

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/MontFerret/ferret/pkg/drivers"
)

const (
	defaultFetchPoolKey  = "default"
	defaultFetchPoolSize = 1
	defaultFetchMaxUses  = 500
	defaultFetchMaxAge   = 30 * time.Minute
	// a tab nobody took for this long is closed, and so is a pool nobody asked for
	defaultFetchMaxIdle = 10 * time.Minute
	// a lease held this long was never returned (the page was not assigned to a variable,
	// so the runtime did not close it): far longer than any query runs
	defaultFetchMaxLease = 10 * time.Minute
	fetchPingTimeout     = 3 * time.Second
	fetchSweepInterval   = time.Minute
)

var errFetchPoolClosed = errors.New("fetchMode pool is closed")

// fetchTab is what the pool needs from a tab. *HTMLPage implements it, tests use a fake.
type fetchTab interface {
	ping(ctx context.Context) error
	destroy() error
}

type fetchEntry struct {
	tab      fetchTab
	uses     int
	created  time.Time
	leased   time.Time
	released time.Time
}

// fetchPool keeps fetchMode tabs of one pool key. A tab serves one query at a time:
// acquire hands out an idle tab or opens a new one while the pool is below its size,
// otherwise it waits until a query returns its tab or ctx expires. A tab that stopped
// answering, served MaxUses queries or outlived MaxAge is closed instead of being reused.
type fetchPool struct {
	mu       sync.Mutex
	size     int
	maxUses  int
	maxAge   time.Duration
	maxIdle  time.Duration
	maxLease time.Duration
	idle     []*fetchEntry
	leased   map[fetchTab]*fetchEntry
	opening  int
	closed   bool
	lastUsed time.Time
	wake     chan struct{}
	now      func() time.Time
}

func newFetchPool(params drivers.FetchParams) *fetchPool {
	pool := &fetchPool{
		size:     params.PoolSize,
		maxUses:  params.MaxUses,
		maxAge:   params.MaxAge,
		maxIdle:  defaultFetchMaxIdle,
		maxLease: defaultFetchMaxLease,
		leased:   make(map[fetchTab]*fetchEntry),
		wake:     make(chan struct{}),
		now:      time.Now,
	}

	if pool.size <= 0 {
		pool.size = defaultFetchPoolSize
	}

	if pool.maxUses <= 0 {
		pool.maxUses = defaultFetchMaxUses
	}

	if pool.maxAge <= 0 {
		pool.maxAge = defaultFetchMaxAge
	}

	pool.lastUsed = pool.now()

	return pool
}

func (pool *fetchPool) acquire(ctx context.Context, open func(ctx context.Context) (fetchTab, error)) (fetchTab, error) {
	for {
		pool.mu.Lock()

		if pool.closed {
			pool.mu.Unlock()

			return nil, errFetchPoolClosed
		}

		pool.lastUsed = pool.now()

		if n := len(pool.idle); n > 0 {
			entry := pool.idle[n-1]
			pool.idle = pool.idle[:n-1]
			// reserved before the ping, so no other query takes it meanwhile
			pool.leased[entry.tab] = entry
			entry.leased = pool.now()
			pool.mu.Unlock()

			// the query gave up while waiting: the tab is fine, it goes back as is
			if err := ctx.Err(); err != nil {
				pool.putBack(entry)

				return nil, errors.Wrap(err, "wait for a free fetchMode tab")
			}

			if pool.expired(entry) || !pool.alive(entry.tab) {
				pool.drop(entry)

				continue
			}

			return entry.tab, nil
		}

		if len(pool.idle)+len(pool.leased)+pool.opening < pool.size {
			pool.opening++
			pool.mu.Unlock()

			return pool.open(ctx, open)
		}

		// every tab is taken: a lease its query never returned frees its slot
		if stale := pool.staleLeasesLocked(); len(stale) > 0 {
			// more slots may be free than this query needs
			pool.notifyLocked()
			pool.mu.Unlock()
			destroyAll(stale)

			continue
		}

		wake := pool.wake
		pool.mu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return nil, errors.Wrap(ctx.Err(), "wait for a free fetchMode tab")
		}
	}
}

func (pool *fetchPool) open(ctx context.Context, open func(ctx context.Context) (fetchTab, error)) (fetchTab, error) {
	tab, err := open(ctx)

	pool.mu.Lock()
	pool.opening--

	if err != nil {
		pool.notifyLocked()
		pool.mu.Unlock()

		return nil, err
	}

	if pool.closed {
		pool.mu.Unlock()
		_ = tab.destroy()

		return nil, errFetchPoolClosed
	}

	now := pool.now()
	pool.leased[tab] = &fetchEntry{tab: tab, created: now, leased: now}
	pool.mu.Unlock()

	return tab, nil
}

// release takes a tab back when its query ends. It returns false for a tab the pool
// does not know, so the caller closes it as an ordinary page.
func (pool *fetchPool) release(tab fetchTab) bool {
	pool.mu.Lock()

	entry, leased := pool.leased[tab]

	if !leased {
		// a repeated Close of a tab that is already back in the pool changes nothing
		idle := pool.isIdleLocked(tab)
		pool.mu.Unlock()

		return idle
	}

	delete(pool.leased, tab)
	entry.uses++
	entry.released = pool.now()
	pool.lastUsed = entry.released

	retire := pool.closed || entry.uses >= pool.maxUses || pool.expired(entry)

	if !retire {
		pool.idle = append(pool.idle, entry)
	}

	pool.notifyLocked()
	pool.mu.Unlock()

	if retire {
		_ = tab.destroy()
	}

	return true
}

// sweep closes idle tabs past MaxAge or idle for longer than maxIdle and leases held
// longer than maxLease. It reports whether the pool is empty and has not been asked for
// within maxIdle: such a pool is closed, and the driver forgets it.
func (pool *fetchPool) sweep() (gone []fetchTab, empty bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	now := pool.now()
	keep := pool.idle[:0]

	for _, entry := range pool.idle {
		if pool.expired(entry) || now.Sub(entry.released) >= pool.maxIdle {
			gone = append(gone, entry.tab)

			continue
		}

		keep = append(keep, entry)
	}

	pool.idle = keep

	gone = append(gone, pool.staleLeasesLocked()...)

	if len(gone) > 0 {
		pool.notifyLocked()
	}

	empty = len(pool.idle) == 0 && len(pool.leased) == 0 && pool.opening == 0 &&
		now.Sub(pool.lastUsed) >= pool.maxIdle

	if empty {
		pool.closed = true
		pool.notifyLocked()
	}

	return gone, empty
}

// close destroys idle tabs. Tabs that are in use are destroyed when their query ends.
func (pool *fetchPool) close() {
	pool.mu.Lock()

	pool.closed = true
	idle := pool.idle
	pool.idle = nil
	pool.notifyLocked()
	pool.mu.Unlock()

	for _, entry := range idle {
		_ = entry.tab.destroy()
	}
}

func (pool *fetchPool) putBack(entry *fetchEntry) {
	pool.mu.Lock()
	delete(pool.leased, entry.tab)

	// the pool may have been closed while the tab was out: nobody would close it then
	closed := pool.closed

	if !closed {
		pool.idle = append(pool.idle, entry)
	}

	pool.notifyLocked()
	pool.mu.Unlock()

	if closed {
		_ = entry.tab.destroy()
	}
}

// touch marks the pool as asked for, so the sweeper does not drop it.
func (pool *fetchPool) touch() {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pool.lastUsed = pool.now()
}

func (pool *fetchPool) drop(entry *fetchEntry) {
	pool.mu.Lock()
	delete(pool.leased, entry.tab)
	pool.notifyLocked()
	pool.mu.Unlock()

	_ = entry.tab.destroy()
}

// staleLeasesLocked forgets leases held longer than maxLease and returns their tabs.
func (pool *fetchPool) staleLeasesLocked() []fetchTab {
	var stale []fetchTab

	now := pool.now()

	for tab, entry := range pool.leased {
		if now.Sub(entry.leased) >= pool.maxLease {
			delete(pool.leased, tab)
			stale = append(stale, tab)
		}
	}

	return stale
}

// alive pings the tab with its own deadline: the query's context may be nearly spent
// after waiting for the tab, and a warmed-up tab must not be dropped for that.
func (pool *fetchPool) alive(tab fetchTab) bool {
	pingCtx, cancel := context.WithTimeout(context.Background(), fetchPingTimeout)
	defer cancel()

	return tab.ping(pingCtx) == nil
}

func (pool *fetchPool) expired(entry *fetchEntry) bool {
	return pool.now().Sub(entry.created) >= pool.maxAge
}

func (pool *fetchPool) isIdleLocked(tab fetchTab) bool {
	for _, entry := range pool.idle {
		if entry.tab == tab {
			return true
		}
	}

	return false
}

// notifyLocked wakes every query waiting for a tab, they re-check the pool themselves.
func (pool *fetchPool) notifyLocked() {
	close(pool.wake)
	pool.wake = make(chan struct{})
}

func destroyAll(tabs []fetchTab) {
	for _, tab := range tabs {
		_ = tab.destroy()
	}
}
