package interpreter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestFIFOOutgoingTranslationOrder(t *testing.T) {
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
	h.commit("user", "첫 문장입니다.", 1)
	<-entered
	h.commit("user", "둘째 문장입니다.", 2)
	if calls.Load() != 1 {
		t.Fatal("outgoing translations overlapped")
	}
	close(release)
	first, second := h.next("tts.request", nil), h.next("tts.request", nil)
	if first["source_text"] != "첫 문장입니다." || second["source_text"] != "둘째 문장입니다." || first["sequence"] != 1 || second["sequence"] != 2 {
		t.Fatalf("out of order: %#v %#v", first, second)
	}
}

func TestStopCancelsLateUserDecisionButKeepsCaptions(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := setup(t, &fakeBackend{judge: func(_ context.Context, in ChunkInput, _ string) (Result, error) {
		if in.Language == "ko-KR" {
			close(entered)
			<-release
		}
		return Result{Text: "R"}, nil
	}})
	h.audio("user", "네 명입니다.", 1, 1)
	h.at.Add(700)
	h.audio("user", "네 명입니다.", 2, 1)
	<-entered
	if err := h.s.StopOutput("stop-1"); err != nil {
		t.Fatal(err)
	}
	h.next("playback.clear", nil)
	close(release)
	h.audio("shop", "少々お待ちください。", 1, 1)
	h.next("caption.update", nil)
	for _, e := range h.seen {
		if e["type"] == "tts.request" {
			t.Fatal("late decision revived output")
		}
	}
}

func TestTranslationFailureBlocksUntilExplicitStopAndMatchingAck(t *testing.T) {
	var calls atomic.Int64
	finished := make(chan struct{}, 3)
	h := setup(t, &fakeBackend{translate: func(_ context.Context, in TranslationInput, _ string) (Result, error) {
		if calls.Add(1) == 1 {
			return Result{}, errors.New("failure")
		}
		defer func() { finished <- struct{}{} }()
		return Result{Text: in.Text}, nil
	}})
	h.commit("user", "첫 문장입니다.", 1)
	h.next("recovery", nil)
	h.commit("user", "둘째 문장입니다.", 2)
	if calls.Load() != 1 {
		t.Fatal("failed sentence skipped")
	}
	_ = h.s.StopOutput("clear-1")
	h.next("playback.clear", nil)
	h.commit("user", "새 문장입니다.", 3)
	<-finished
	// finished를 받아도 세션이 번역 결과를 대기열에 넣기 전일 수 있다.
	// 새 음성이 대기열에 들어가야 잘못된 중단 확인을 받고도 그대로 대기하는지 검사할 수 있다.
	deadline := time.Now().Add(time.Second)
	for {
		h.s.mu.Lock()
		held := len(h.s.held)
		h.s.mu.Unlock()
		if held == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("speech not held")
		}
		time.Sleep(time.Millisecond)
	}
	h.s.Cleared("wrong")
	h.s.mu.Lock()
	if len(h.s.held) != 1 {
		t.Fatal("wrong clear released audio")
	}
	h.s.mu.Unlock()
	h.s.Cleared("clear-1")
	e := h.next("tts.request", nil)
	if e["generation"] != 1 || e["source_text"] != "새 문장입니다." {
		t.Fatal(e)
	}
	if err := h.s.StopOutput("clear-1"); err != nil {
		t.Fatal(err)
	}
	h.s.mu.Lock()
	if h.s.generation != 1 {
		t.Fatal("duplicate stop changed generation")
	}
	h.s.mu.Unlock()
}
