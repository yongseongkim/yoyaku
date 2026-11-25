package interpreter

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSyllablesRequireAgreementBeforeModelAndAudio(t *testing.T) {
	var calls atomic.Int64
	b := &fakeBackend{judge: func(_ context.Context, in ChunkInput, _ string) (Result, error) {
		calls.Add(1)
		if in.Text != "리베라 시부야입니다." {
			t.Errorf("premature source: %s", in.Text)
		}
		return Result{Text: "R"}, nil
	}}
	h := setup(t, b)
	for i, text := range []string{"리", "리베", "리베라", "리베라 시부야입니다."} {
		h.audio("user", text, int64(i+1), 1)
		h.at.Add(30)
	}
	if calls.Load() != 0 {
		t.Fatal("syllables reached judge")
	}
	h.at.Add(700)
	h.audio("user", "리베라 시부야입니다.", 5, 1)
	e := h.next("tts.request", nil)
	if e["source_text"] != "리베라 시부야입니다." || calls.Load() != 1 {
		t.Fatalf("unexpected output: %#v", e)
	}
	before := calls.Load()
	h.audio("user", "리베라 시부야입니다.", 6, 1)
	if calls.Load() != before {
		t.Fatal("retired source replayed")
	}
}

func TestIncompleteConditionWaitsThenExpands(t *testing.T) {
	h := setup(t, &fakeBackend{judge: func(_ context.Context, in ChunkInput, _ string) (Result, error) {
		label := "W"
		if strings.HasSuffix(in.Text, "예약하고 싶어요.") {
			label = "R"
		}
		return Result{Text: label}, nil
	}})
	h.audio("user", "취소 수수료가 없으면", 1, 1)
	h.at.Add(700)
	h.audio("user", "취소 수수료가 없으면", 2, 1)
	if e := h.next("chunk.decision", nil); e["label"] != "W" {
		t.Fatal(e)
	}
	h.at.Add(5000)
	h.audio("user", "취소 수수료가 없으면", 3, 1)
	h.s.mu.Lock()
	if len(h.s.userQueue) != 0 || h.s.userJob != nil {
		t.Fatal("quiet forced incomplete speech")
	}
	h.s.mu.Unlock()
	h.audio("user", "취소 수수료가 없으면 예약하고 싶어요.", 4, 1)
	h.at.Add(700)
	h.audio("user", "취소 수수료가 없으면 예약하고 싶어요.", 5, 1)
	if e := h.next("tts.request", nil); e["source_text"] != "취소 수수료가 없으면 예약하고 싶어요." {
		t.Fatal(e)
	}
}

func TestPrefixCommitPreservesTrailingAudioAndQuietWholeWindow(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprint(quiet), func(t *testing.T) {
			text := "11월 14일입니다. 인원은"
			if quiet {
				text = "김민수입니다."
			}
			h := setup(t, &fakeBackend{recognize: func(context.Context, []byte, string, bool) (Recognition, error) {
				words := []Word{{Word: "11월 14일입니다.", End: 1}, {Word: "인원은", End: 2.5}}
				if quiet {
					words = []Word{{Word: text, End: 1}}
				}
				return Recognition{Text: text, Words: words}, nil
			}})
			for seq := int64(1); seq <= 2; seq++ {
				meta := Audio{Track: "user", WindowID: "user-1", Sequence: seq, Samples: 48000}
				if quiet {
					meta.QuietMS = 900
				}
				if err := h.s.Audio(meta, []byte(text)); err != nil {
					t.Fatal(err)
				}
				h.next("audio.processed", nil)
				h.at.Add(700)
			}
			e := h.next("audio.consume", nil)
			want := 16000
			if quiet {
				want = 48000
			}
			if e["through_samples"] != want {
				t.Fatalf("consumed %#v, want %d", e, want)
			}
		})
	}
}

func TestCorrectionInvalidatesLateReadyResult(t *testing.T) {
	first := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	h := setup(t, &fakeBackend{judge: func(_ context.Context, _ ChunkInput, _ string) (Result, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-first
		}
		return Result{Text: "R"}, nil
	}})
	h.audio("user", "두 명입니다.", 1, 1)
	h.at.Add(700)
	h.audio("user", "두 명입니다.", 2, 1)
	<-entered
	h.audio("user", "네 명입니다.", 3, 1)
	h.at.Add(700)
	h.audio("user", "네 명입니다.", 4, 1)
	close(first)
	e := h.next("tts.request", nil)
	if e["source_text"] != "네 명입니다." {
		t.Fatal(e)
	}
}

func TestCorrectionCueAndMissingAlignment(t *testing.T) {
	text := "두 명입니다. 아니 네 명입니다."
	cs := candidates(text, []Word{{Word: "두 명입니다.", End: 1}, {Word: "아니 네 명입니다.", End: 2}}, 48000)
	if len(cs) != 1 || cs[0].text != text {
		t.Fatalf("correction split: %#v", cs)
	}
	if len(candidates(text, nil, 48000)) != 0 {
		t.Fatal("unaligned source accepted")
	}
}

func TestInvalidLabelAndMissingWordTimesNeverAuthorizeSpeech(t *testing.T) {
	for _, label := range []string{"P", "", "R W"} {
		t.Run(label, func(t *testing.T) {
			h := setup(t, &fakeBackend{judge: func(context.Context, ChunkInput, string) (Result, error) { return Result{Text: label}, nil }})
			h.audio("user", "네 명입니다.", 1, 1)
			h.at.Add(700)
			h.audio("user", "네 명입니다.", 2, 1)
			h.next("recovery", nil)
			h.s.mu.Lock()
			defer h.s.mu.Unlock()
			if h.s.userJob != nil || !h.s.windows["user"].blocked {
				t.Fatal("invalid model output accepted")
			}
		})
	}
	h := setup(t, &fakeBackend{recognize: func(context.Context, []byte, string, bool) (Recognition, error) {
		return Recognition{Text: "네 명입니다."}, nil
	}})
	h.audio("user", "ignored", 1, 1)
	found := false
	for _, e := range h.seen {
		if e["type"] == "recovery" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing alignment hidden")
	}
}
