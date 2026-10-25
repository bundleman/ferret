package network

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/mafredri/cdp/protocol/page"
	"github.com/rs/zerolog"
	. "github.com/smartystreets/goconvey/convey"
)

const navigationStreamFrame = "network.(*NavigationEventStream).Read.func1"

type (
	fakeFrameNavigated struct {
		ready chan struct{}
		recv  func() (*page.FrameNavigatedReply, error)
	}

	fakeNavigatedWithinDocument struct {
		ready chan struct{}
		recv  func() (*page.NavigatedWithinDocumentReply, error)
	}
)

func (s *fakeFrameNavigated) Ready() <-chan struct{} {
	return s.ready
}

func (s *fakeFrameNavigated) RecvMsg(interface{}) error {
	return nil
}

func (s *fakeFrameNavigated) Close() error {
	return nil
}

func (s *fakeFrameNavigated) Recv() (*page.FrameNavigatedReply, error) {
	return s.recv()
}

func (s *fakeNavigatedWithinDocument) Ready() <-chan struct{} {
	return s.ready
}

func (s *fakeNavigatedWithinDocument) RecvMsg(interface{}) error {
	return nil
}

func (s *fakeNavigatedWithinDocument) Close() error {
	return nil
}

func (s *fakeNavigatedWithinDocument) Recv() (*page.NavigatedWithinDocumentReply, error) {
	return s.recv()
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

func silentFrame() *fakeFrameNavigated {
	return &fakeFrameNavigated{ready: make(chan struct{})}
}

func silentDoc() *fakeNavigatedWithinDocument {
	return &fakeNavigatedWithinDocument{ready: make(chan struct{})}
}

func TestNavigationEventStream(t *testing.T) {
	Convey("NavigationEventStream", t, func() {
		// WaitForNavigation returns, cancels ctx and closes the stream while the goroutine
		// waits in Recv: Recv then fails and nobody reads the error
		Convey("Should not leak the reading goroutine when a frame stream fails after its reader is gone", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			entered := make(chan struct{})
			frame := &fakeFrameNavigated{ready: make(chan struct{}, 1)}
			frame.recv = func() (*page.FrameNavigatedReply, error) {
				close(entered)
				<-ctx.Done()

				return nil, errors.New("stream is closed")
			}
			frame.ready <- struct{}{}

			before := goroutinesWith(navigationStreamFrame)
			_ = newNavigationEventStream(zerolog.Nop(), nil, frame, silentDoc()).Read(ctx)

			<-entered
			cancel()

			So(leakedSince(before, navigationStreamFrame, 2*time.Second), ShouldEqual, 0)
		})

		// the reader takes one event and stops reading, as the wait for a document commit
		// does; the page navigates once more before ctx is done
		Convey("Should not leak the reading goroutine when its reader takes the first frame event and leaves", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			second := make(chan struct{})
			calls := 0
			frame := &fakeFrameNavigated{ready: make(chan struct{}, 2)}
			frame.recv = func() (*page.FrameNavigatedReply, error) {
				calls++

				if calls == 2 {
					close(second)
				}

				return &page.FrameNavigatedReply{Frame: page.Frame{ID: "main", URL: "https://example.com/"}}, nil
			}
			frame.ready <- struct{}{}
			frame.ready <- struct{}{}

			before := goroutinesWith(navigationStreamFrame)
			ch := newNavigationEventStream(zerolog.Nop(), nil, frame, silentDoc()).Read(ctx)

			evt := <-ch
			So(evt.Err(), ShouldBeNil)

			<-second
			cancel()

			So(leakedSince(before, navigationStreamFrame, 2*time.Second), ShouldEqual, 0)
		})

		Convey("Should not leak the reading goroutine when a within-document stream fails after its reader is gone", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			entered := make(chan struct{})
			doc := &fakeNavigatedWithinDocument{ready: make(chan struct{}, 1)}
			doc.recv = func() (*page.NavigatedWithinDocumentReply, error) {
				close(entered)
				<-ctx.Done()

				return nil, errors.New("stream is closed")
			}
			doc.ready <- struct{}{}

			before := goroutinesWith(navigationStreamFrame)
			_ = newNavigationEventStream(zerolog.Nop(), nil, silentFrame(), doc).Read(ctx)

			<-entered
			cancel()

			So(leakedSince(before, navigationStreamFrame, 2*time.Second), ShouldEqual, 0)
		})

		// a single-page app changes its URL with history.pushState after the reader has
		// taken the navigation it waited for
		Convey("Should not leak the reading goroutine when its reader takes the first within-document event and leaves", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			second := make(chan struct{})
			calls := 0
			doc := &fakeNavigatedWithinDocument{ready: make(chan struct{}, 2)}
			doc.recv = func() (*page.NavigatedWithinDocumentReply, error) {
				calls++

				if calls == 2 {
					close(second)
				}

				return &page.NavigatedWithinDocumentReply{FrameID: "main", URL: "https://example.com/#next"}, nil
			}
			doc.ready <- struct{}{}
			doc.ready <- struct{}{}

			before := goroutinesWith(navigationStreamFrame)
			ch := newNavigationEventStream(zerolog.Nop(), nil, silentFrame(), doc).Read(ctx)

			evt := <-ch
			So(evt.Err(), ShouldBeNil)

			<-second
			cancel()

			So(leakedSince(before, navigationStreamFrame, 2*time.Second), ShouldEqual, 0)
		})
	})
}
