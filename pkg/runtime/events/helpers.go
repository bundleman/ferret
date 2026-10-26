package events

import (
	"context"
	"sync"

	"github.com/MontFerret/ferret/pkg/runtime/core"
)

type mrger struct {
	inputs []Stream
}

func (m *mrger) Close(ctx context.Context) error {
	errs := make([]error, 0, len(m.inputs))

	for _, s := range m.inputs {
		if err := s.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return core.Errors(errs...)
}

func (m *mrger) Read(ctx context.Context) <-chan Message {
	var wg sync.WaitGroup
	wg.Add(len(m.inputs))

	out := make(chan Message)
	consume := func(c context.Context, input Stream) {
		for evt := range input.Read(c) {
			out <- evt
		}

		wg.Done()
	}

	for _, ch := range m.inputs {
		go consume(ctx, ch)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func Merge(inputs ...Stream) Stream {
	return &mrger{inputs}
}

// Send passes msg to a stream reader and reports whether it was taken. A reader that
// went away after ctx was done no longer blocks the sending goroutine forever: the
// goroutine of a stream nobody reads exits with ctx instead of leaking. A reader that
// is already waiting still gets msg, even if ctx is done by then.
func Send(ctx context.Context, ch chan<- Message, msg Message) bool {
	// select picks a ready case at random, so try the send alone first: a waiting reader
	// wins over a done ctx
	select {
	case ch <- msg:
		return true
	default:
	}

	select {
	case ch <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}
