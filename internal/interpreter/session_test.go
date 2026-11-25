package interpreter

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestInvalidAndShrinkingMetadataRejectedBeforeASR(t *testing.T) {
	h := setup(t, &fakeBackend{})
	for _, meta := range []Audio{{Track: "wrong", WindowID: "user-1", Sequence: 1, Samples: 32000}, {Track: "user", WindowID: "user-0", Sequence: 1, Samples: 32000}, {Track: "user", WindowID: "user-01", Sequence: 1, Samples: 32000}} {
		if h.s.Audio(meta, nil) == nil {
			t.Fatal("bad metadata accepted")
		}
	}
	h.audio("user", "네 명입니다.", 1, 1)
	if h.s.Audio(Audio{Track: "user", WindowID: "user-2", Sequence: 1, Samples: 32000}, nil) == nil {
		t.Fatal("unconsumed audio replaced")
	}
	if h.s.Audio(Audio{Track: "user", WindowID: "user-1", Sequence: 2, Samples: 16000}, nil) == nil {
		t.Fatal("window shrank")
	}
}

func TestRecognitionTimeoutAndCloseCancelWorker(t *testing.T) {
	for _, closeEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(closeEarly), func(t *testing.T) {
			entered, done := make(chan struct{}), make(chan struct{})
			h := setup(t, &fakeBackend{recognize: func(ctx context.Context, _ []byte, _ string, _ bool) (Recognition, error) {
				close(entered)
				<-ctx.Done()
				close(done)
				return Recognition{}, ctx.Err()
			}})
			h.s.opts.Deadline = 10 * time.Millisecond
			if err := h.s.Audio(Audio{Track: "user", WindowID: "user-1", Sequence: 1, Samples: 32000}, nil); err != nil {
				t.Fatal(err)
			}
			<-entered
			if closeEarly {
				h.s.Close()
				<-done
				h.s.mu.Lock()
				if !h.s.closed {
					t.Fatal("not closed")
				}
				h.s.mu.Unlock()
			} else {
				h.next("recovery", nil)
				<-done
			}
		})
	}
}

func TestOutputBackpressureClosesWithoutDroppingSilently(t *testing.T) {
	h := setup(t, &fakeBackend{})
	h.s.mu.Lock()
	for i := 0; i < 130; i++ {
		h.s.emit(Event{"type": "test"})
	}
	closed := h.s.closed
	h.s.mu.Unlock()
	if !closed {
		t.Fatal("unbounded output")
	}
}

func TestLateRecognitionAfterDeadlineCannotReviveSource(t *testing.T) {
	h := setup(t, &fakeBackend{recognize: func(ctx context.Context, _ []byte, _ string, _ bool) (Recognition, error) {
		<-ctx.Done()
		return Recognition{Text: "네 명입니다.", Words: []Word{{Word: "네 명입니다.", End: 1}}}, nil
	}})
	h.s.opts.Deadline = 5 * time.Millisecond
	if err := h.s.Audio(Audio{Track: "user", WindowID: "user-1", Sequence: 1, Samples: 32000, QuietMS: 900}, nil); err != nil {
		t.Fatal(err)
	}
	h.next("audio.processed", nil)
	found := false
	for _, event := range h.seen {
		if event["type"] == "transcript" {
			t.Fatal("expired recognition accepted")
		}
		if event["type"] == "recovery" {
			found = true
		}
	}
	if !found {
		t.Fatal("recognition deadline was hidden")
	}
}
