package network

import (
	"testing"

	"github.com/rs/zerolog"
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

// TestNavigationEventStreamRelease: Release stops both subscriptions without waiting for the
// document, also when no event has been read, which Close does not do.
func TestNavigationEventStreamRelease(t *testing.T) {
	onFrame := &countingFrameNavigated{fakeFrameNavigated: &fakeFrameNavigated{ready: make(chan struct{})}}
	onDoc := &countingNavigatedWithinDocument{fakeNavigatedWithinDocument: &fakeNavigatedWithinDocument{ready: make(chan struct{})}}

	stream := newNavigationEventStream(zerolog.Nop(), nil, onFrame, onDoc)

	if err := stream.Release(); err != nil {
		t.Fatal(err)
	}

	if onFrame.closed != 1 || onDoc.closed != 1 {
		t.Errorf("closed frame %d and document %d streams, want both once", onFrame.closed, onDoc.closed)
	}
}
