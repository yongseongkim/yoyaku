package interpreter

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	recognize func(context.Context, []byte, string, bool) (Recognition, error)
	judge     func(context.Context, ChunkInput, string) (Result, error)
	translate func(context.Context, TranslationInput, string) (Result, error)
}

func (f *fakeBackend) Transcribe(ctx context.Context, data []byte, lang string, times bool) (Recognition, error) {
	if f.recognize != nil {
		return f.recognize(ctx, data, lang, times)
	}
	text := string(data)
	return Recognition{Text: text, Words: []Word{{Word: text, End: 1}}}, nil
}

func (f *fakeBackend) Judge(ctx context.Context, in ChunkInput, model string) (Result, error) {
	if f.judge != nil {
		return f.judge(ctx, in, model)
	}
	return Result{Text: "R", PromptVersion: "chunk-readiness-v1"}, nil
}

func (f *fakeBackend) Translate(ctx context.Context, in TranslationInput, model string) (Result, error) {
	if f.translate != nil {
		return f.translate(ctx, in, model)
	}
	return Result{Text: "translated:" + in.Text}, nil
}

type harness struct {
	s    *Session
	at   atomic.Int64
	seen []Event
	t    *testing.T
}

func setup(t *testing.T, b *fakeBackend) *harness {
	t.Helper()
	h := &harness{t: t}
	h.s = New(context.Background(), b, "gemma26b", Options{Now: func() time.Time { return time.UnixMilli(h.at.Load()) }, CaptionDebounce: time.Millisecond, CaptionMaxWait: 5 * time.Millisecond, Deadline: time.Second})
	t.Cleanup(h.s.Close)
	return h
}

func (h *harness) next(kind string, predicate func(Event) bool) Event {
	h.t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-h.s.Events():
			h.seen = append(h.seen, e)
			if e["type"] == kind && (predicate == nil || predicate(e)) {
				return e
			}
		case <-timer.C:
			h.t.Fatalf("missing %s; observed: %#v", kind, h.seen)
			return nil
		}
	}
}

func (h *harness) audio(track, text string, seq int64, number int64) {
	h.t.Helper()
	meta := Audio{Track: track, WindowID: fmt.Sprintf("%s-%d", track, number), Sequence: seq, Samples: 32000, QuietMS: 900}
	if err := h.s.Audio(meta, []byte(text)); err != nil {
		h.t.Fatal(err)
	}
	h.next("audio.processed", func(e Event) bool { return e["track"] == track && e["sequence"] == seq })
}

func (h *harness) commit(track, text string, number int64) {
	h.t.Helper()
	h.audio(track, text, 1, number)
	h.at.Add(700)
	h.audio(track, text, 2, number)
	h.next("source.commit", func(e Event) bool {
		return e["track"] == track && e["window_id"] == fmt.Sprintf("%s-%d", track, number)
	})
}
