package network

import (
	"testing"

	"github.com/rs/zerolog"
	. "github.com/smartystreets/goconvey/convey"
)

type (
	countingFrameNavigated struct {
		*fakeFrameNavigated
		closed int
	}

	countingNavigatedWithinDocument struct {
		*fakeNavigatedWithinDocument
		closed int
	}
)

func (s *countingFrameNavigated) Close() error {
	s.closed++

	return nil
}

func (s *countingNavigatedWithinDocument) Close() error {
	s.closed++

	return nil
}

func Test_NavigationEventStream_Release(t *testing.T) {
	// Unlike Close, Release does not wait for the document, so it works before any event is read.
	Convey("Should stop both subscriptions once without waiting for the document", t, func() {
		onFrame := &countingFrameNavigated{fakeFrameNavigated: &fakeFrameNavigated{ready: make(chan struct{})}}
		onDoc := &countingNavigatedWithinDocument{fakeNavigatedWithinDocument: &fakeNavigatedWithinDocument{ready: make(chan struct{})}}

		stream := newNavigationEventStream(zerolog.Nop(), nil, onFrame, onDoc)

		So(stream.Release(), ShouldBeNil)
		So(onFrame.closed, ShouldEqual, 1)
		So(onDoc.closed, ShouldEqual, 1)
	})
}
