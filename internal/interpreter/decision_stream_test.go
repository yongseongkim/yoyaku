package interpreter

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"yoyaku/internal/decision"
)

// 인식 원문과 모델의 응답을 미리 정해 두고, 실제 제어기가 언제 판단을 요청하고 원문을 확정하는지 확인한다.
func TestDecisionStreams(t *testing.T) {
	catalog, e := decision.Load("../..")
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range catalog.Cases {
		t.Run(c.ID, func(t *testing.T) {
			var calls atomic.Int64
			labels := map[string]string{}
			for _, v := range c.Snapshots {
				if v.Expected != "-" {
					labels[v.Text] = v.Expected
				}
			}
			h := setup(t, &fakeBackend{recognize: func(_ context.Context, b []byte, _ string, _ bool) (Recognition, error) {
				var v decision.Snapshot
				if e := json.Unmarshal(b, &v); e != nil {
					return Recognition{}, e
				}
				return Recognition{Text: v.Text, Words: []Word{{Word: v.Text, End: float64(v.WordEndMS) / 1000}}}, nil
			}, judge: func(_ context.Context, in ChunkInput, _ string) (Result, error) {
				calls.Add(1)
				label, ok := labels[in.Text]
				if !ok {
					t.Errorf("Unstable/unexpected source reached model: %q", in.Text)
					return Result{Text: "W"}, nil
				}
				return Result{Text: label}, nil
			}})
			for i, v := range c.Snapshots {
				before := calls.Load()
				seenBefore := len(h.seen)
				h.at.Store(v.AtMS)
				b, _ := json.Marshal(v)
				meta := Audio{Track: c.Track, WindowID: c.Track + "-1", Sequence: int64(i + 1), Samples: v.Samples, QuietMS: v.QuietMS}
				if e := h.s.Audio(meta, b); e != nil {
					t.Fatal(e)
				}
				h.next("audio.processed", func(e Event) bool { return e["sequence"] == int64(i+1) })
				if v.Expected == "-" {
					for _, event := range h.seen[seenBefore:] {
						if event["type"] == "chunk.request" {
							t.Fatalf("@%dms ineligible source emitted a model request", v.AtMS)
						}
					}
					if calls.Load() != before {
						t.Fatalf("@%dms partial source invoked the model", v.AtMS)
					}
					continue
				}
				event := h.next("chunk.decision", nil)
				if event["label"] != v.Expected || event["text"] != v.Text {
					t.Fatal(event)
				}
				if calls.Load() != before+1 {
					t.Fatal("Decision count changed")
				}
				if v.Commit != "" {
					commit := h.next("source.commit", nil)
					if commit["text"] != v.Commit {
						t.Fatal("Wrong committed source", commit)
					}
				} else {
					h.s.mu.Lock()
					w := h.s.windows[c.Track]
					if w == nil {
						t.Error("W consumed the source")
					}
					h.s.mu.Unlock()
				}
			}
		})
	}
}
