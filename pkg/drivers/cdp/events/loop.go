package events

import (
	"context"
	"math/rand"
	"sync"

	"github.com/MontFerret/ferret/pkg/runtime/core"
)

type Loop struct {
	mu        sync.RWMutex
	sources   *SourceCollection
	listeners *ListenerCollection
	cancel    context.CancelFunc
}

func NewLoop() *Loop {
	loop := new(Loop)
	loop.sources = NewSourceCollection()
	loop.listeners = NewListenerCollection()

	return loop
}

func (loop *Loop) Run(ctx context.Context) error {
	loop.mu.Lock()
	defer loop.mu.Unlock()

	if loop.cancel != nil {
		return core.Error(core.ErrInvalidOperation, "loop is already running")
	}

	childCtx, cancel := context.WithCancel(ctx)

	loop.cancel = cancel

	go loop.run(childCtx)

	return nil
}

func (loop *Loop) Close() error {
	loop.mu.Lock()
	defer loop.mu.Unlock()

	if loop.cancel != nil {
		loop.cancel()
		loop.cancel = nil
	}

	return loop.sources.Close()
}

func (loop *Loop) AddSource(source Source) {
	loop.mu.RLock()
	defer loop.mu.RUnlock()

	loop.sources.Add(source)
}

func (loop *Loop) RemoveSource(source Source) {
	loop.mu.RLock()
	defer loop.mu.RUnlock()

	loop.sources.Remove(source)
}

func (loop *Loop) Listeners(eventID ID) int {
	loop.mu.RLock()
	defer loop.mu.RUnlock()

	return loop.listeners.Size(eventID)
}

func (loop *Loop) AddListener(eventID ID, handler Handler) ListenerID {
	loop.mu.RLock()
	defer loop.mu.RUnlock()

	listener := Listener{
		ID:      ListenerID(rand.Int()),
		EventID: eventID,
		Handler: handler,
	}

	loop.listeners.Add(listener)

	return listener.ID
}

func (loop *Loop) RemoveListener(eventID ID, listenerID ListenerID) {
	loop.mu.RLock()
	defer loop.mu.RUnlock()

	loop.listeners.Remove(eventID, listenerID)
}

// run starts running an event loop.
// It constantly iterates over each event source.
func (loop *Loop) run(ctx context.Context) {
	sources := loop.sources
	size := sources.Size()
	counter := -1

	for {
		if isCtxDone(ctx) {
			break
		}

		counter++

		if counter >= size {
			// reset the counter
			size = sources.Size()
			counter = 0
		}

		var source Source

		if size > 0 {
			found, err := sources.Get(counter)

			if err == nil {
				source = found
			} else {
				// force to reset counter
				counter = size
				continue
			}
		} else {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-source.Ready():
			if isCtxDone(ctx) {
				return
			}

			event, err := source.Recv()

			loop.emit(ctx, event.ID, event.Data, err)
		default:
			continue
		}
	}
}

func (loop *Loop) emit(ctx context.Context, eventID ID, message interface{}, err error) {
	if err != nil {
		eventID = Error
		message = err
	}

	loop.mu.RLock()
	snapshot := loop.listeners.Values(eventID)
	loop.mu.RUnlock()

	for _, listener := range snapshot {
		select {
		case <-ctx.Done():
			return
		default:
			// if returned false, it means the loops should not call the handler anymore
			if !listener.Handler(ctx, message) {
				loop.mu.RLock()
				loop.listeners.Remove(eventID, listener.ID)
				loop.mu.RUnlock()
			}
		}
	}
}
