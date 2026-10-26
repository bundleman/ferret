package events_test

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"

	events2 "github.com/MontFerret/ferret/pkg/drivers/cdp/events"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/events"
	"github.com/MontFerret/ferret/pkg/runtime/values"

	. "github.com/smartystreets/goconvey/convey"
)

const eventStreamFrame = "events.(*EventStream).Read.func1"

type (
	TestStream struct {
		mock.Mock
		ready   chan struct{}
		message chan events.Message
	}
)

func NewTestStream() *TestStream {
	return NewBufferedTestStream(0)
}

func NewBufferedTestStream(buffer int) *TestStream {
	es := new(TestStream)
	es.ready = make(chan struct{}, buffer)
	es.message = make(chan events.Message, buffer)
	return es
}

func (ts *TestStream) Ready() <-chan struct{} {
	return ts.ready
}

func (ts *TestStream) RecvMsg(m interface{}) error {
	return nil
}

func (ts *TestStream) Close() error {
	ts.Called()
	close(ts.message)
	close(ts.ready)
	return nil
}

func (ts *TestStream) Emit(val core.Value) {
	ts.ready <- struct{}{}
	ts.message <- events.WithValue(val)
}

func (ts *TestStream) EmitError(err error) {
	ts.ready <- struct{}{}
	ts.message <- events.WithErr(err)
}

func (ts *TestStream) Recv() (core.Value, error) {
	msg := <-ts.message

	return msg.Value(), msg.Err()
}

func TestStreamReader(t *testing.T) {
	Convey("StreamReader", t, func() {
		Convey("Should read data from Stream", func() {
			ctx, cancel := context.WithCancel(context.Background())

			stream := NewTestStream()
			stream.On("Close", mock.Anything).Maybe().Return(nil)

			go func() {
				stream.Emit(values.NewString("foo"))
				stream.Emit(values.NewString("bar"))
				stream.Emit(values.NewString("baz"))
			}()

			data := make([]string, 0, 3)

			es := events2.NewEventStream(stream, func(_ context.Context, stream rpcc.Stream) (core.Value, error) {
				return stream.(*TestStream).Recv()
			})

			ch := es.Read(ctx)

			// ctx is done only after the reader took everything: a message sent once
			// ctx is done is not guaranteed to be delivered anymore
			for len(data) < 3 {
				evt, ok := <-ch
				So(ok, ShouldBeTrue)
				So(evt.Err(), ShouldBeNil)
				So(evt.Value(), ShouldNotBeNil)

				data = append(data, evt.Value().String())
			}

			cancel()

			for range ch {
			}

			So(data, ShouldResemble, []string{"foo", "bar", "baz"})

			stream.AssertExpectations(t)

			So(es.Close(context.Background()), ShouldBeNil)
		})

		Convey("Should handle error but do not close Stream", func() {
			ctx := context.Background()

			stream := NewTestStream()
			stream.On("Close", mock.Anything).Maybe().Return(nil)

			go func() {
				stream.EmitError(errors.New("foo"))
			}()

			reader := events2.NewEventStream(stream, func(_ context.Context, stream rpcc.Stream) (core.Value, error) {
				return stream.(*TestStream).Recv()
			})

			ch := reader.Read(ctx)
			evt := <-ch
			So(evt.Err(), ShouldNotBeNil)

			time.Sleep(time.Duration(100) * time.Millisecond)

			stream.AssertExpectations(t)
		})

		Convey("Should not close Stream when Context is cancelled", func() {
			stream := NewTestStream()
			stream.On("Close", mock.Anything).Maybe().Return(nil)

			reader := events2.NewEventStream(stream, func(_ context.Context, stream rpcc.Stream) (core.Value, error) {
				return values.EmptyArray(), nil
			})

			ctx, cancel := context.WithCancel(context.Background())

			_ = reader.Read(ctx)

			time.Sleep(time.Duration(100) * time.Millisecond)

			cancel()

			time.Sleep(time.Duration(100) * time.Millisecond)

			stream.AssertExpectations(t)
		})

		Convey("Should not leak the reading goroutine when nobody reads", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stream := NewBufferedTestStream(1)
			stream.On("Close", mock.Anything).Maybe().Return(nil)
			stream.Emit(values.NewString("foo"))

			decoded := make(chan struct{})
			var once sync.Once
			before := goroutinesWith(eventStreamFrame)

			reader := events2.NewEventStream(stream, func(_ context.Context, stream rpcc.Stream) (core.Value, error) {
				defer once.Do(func() { close(decoded) })

				return stream.(*TestStream).Recv()
			})

			_ = reader.Read(ctx)

			<-decoded
			So(leakedSince(before, eventStreamFrame, 0), ShouldEqual, 1)
			cancel()

			So(leakedSince(before, eventStreamFrame, 2*time.Second), ShouldEqual, 0)
		})
	})
}

// goroutinesWith returns the ids of goroutines whose stack contains frame.
func goroutinesWith(frame string) map[string]struct{} {
	buf := make([]byte, 1<<16)

	for {
		n := runtime.Stack(buf, true)

		if n < len(buf) {
			buf = buf[:n]

			break
		}

		buf = make([]byte, 2*len(buf))
	}

	ids := make(map[string]struct{})

	for _, block := range bytes.Split(buf, []byte("\n\n")) {
		if !bytes.Contains(block, []byte(frame)) {
			continue
		}

		header := bytes.Fields(block)

		if len(header) > 1 {
			ids[string(header[1])] = struct{}{}
		}
	}

	return ids
}

// leakedSince waits until every goroutine with frame that was not running before is
// gone, up to the timeout, and returns how many are left. The stream channel is not
// read meanwhile: a read would unblock a leaked send and hide the leak.
func leakedSince(before map[string]struct{}, frame string, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)

	for {
		left := 0

		for id := range goroutinesWith(frame) {
			if _, old := before[id]; !old {
				left++
			}
		}

		if left == 0 || time.Now().After(deadline) {
			return left
		}

		time.Sleep(5 * time.Millisecond)
	}
}
