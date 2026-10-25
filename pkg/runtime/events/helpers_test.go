package events_test

import (
	"context"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/runtime/events"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

func TestSend(t *testing.T) {
	Convey("Send", t, func() {
		Convey("Should hand the message over when the channel can take it, even if the context is done", func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			// a channel with room behaves as a waiting reader; without the attempt that
			// comes before the context check, select would pick a side at random
			for i := 0; i < 1000; i++ {
				ch := make(chan events.Message, 1)

				So(events.Send(ctx, ch, events.WithValue(values.NewInt(i))), ShouldBeTrue)
				So((<-ch).Value(), ShouldEqual, values.NewInt(i))
			}
		})

		Convey("Should give up when nobody reads and the context is done", func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			ch := make(chan events.Message)

			So(events.Send(ctx, ch, events.WithValue(values.True)), ShouldBeFalse)
		})

		Convey("Should wait for a reader while the context is alive", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ch := make(chan events.Message)
			taken := make(chan bool, 1)

			go func() {
				taken <- events.Send(ctx, ch, events.WithValue(values.True))
			}()

			select {
			case msg := <-ch:
				So(msg.Value(), ShouldEqual, values.True)
			case <-time.After(2 * time.Second):
				So("the message did not arrive", ShouldBeEmpty)
			}

			So(<-taken, ShouldBeTrue)
		})

		Convey("Should stop waiting for a reader once the context is done", func() {
			ctx, cancel := context.WithCancel(context.Background())

			ch := make(chan events.Message)
			taken := make(chan bool, 1)

			go func() {
				taken <- events.Send(ctx, ch, events.WithValue(values.True))
			}()

			cancel()

			select {
			case ok := <-taken:
				So(ok, ShouldBeFalse)
			case <-time.After(2 * time.Second):
				So("Send is still blocked", ShouldBeEmpty)
			}
		})
	})
}
