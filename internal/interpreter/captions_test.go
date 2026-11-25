package interpreter

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestProvisionalCaptionsDoNotWaitForReadiness(t *testing.T) {
	var lang string
	h := setup(t, &fakeBackend{recognize: func(_ context.Context, data []byte, l string, times bool) (Recognition, error) {
		lang = l
		if !times {
			t.Error("missing word times")
		}
		return Recognition{Text: string(data), Words: []Word{{Word: string(data), End: 1}}}, nil
	}, judge: func(context.Context, ChunkInput, string) (Result, error) { return Result{Text: "W"}, nil }})
	h.audio("shop", "ご予約はまだ", 1, 1)
	e := h.next("caption.update", nil)
	if e["provisional"] != true || lang != "ja" {
		t.Fatalf("missing Japanese provisional caption: %#v", e)
	}
}

func TestCaptionReplacementDiscardsLateTranslation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	h := setup(t, &fakeBackend{translate: func(_ context.Context, in TranslationInput, _ string) (Result, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return Result{Text: in.Text}, nil
	}})
	h.audio("shop", "2名です。", 1, 1)
	<-entered
	h.audio("shop", "4名です。", 2, 1)
	close(release)
	e := h.next("caption.update", nil)
	if e["text"] != "4名です。" {
		t.Fatalf("obsolete caption displayed: %#v", e)
	}
	h.audio("shop", "2名です。", 3, 1)
	found := false
	for _, event := range h.seen {
		if event["type"] == "caption.invalidate" {
			found = true
		}
	}
	if !found {
		t.Fatal("replacement did not invalidate the displayed caption")
	}
}
