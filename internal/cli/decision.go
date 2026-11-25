package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
	"yoyaku/internal/decision"
	"yoyaku/internal/interpreter"
	"yoyaku/internal/worker"
)

type replayBackend struct{ client *worker.Client }

func (b replayBackend) Transcribe(_ context.Context, data []byte, _ string, _ bool) (interpreter.Recognition, error) {
	var v decision.Snapshot
	if e := json.Unmarshal(data, &v); e != nil {
		return interpreter.Recognition{}, e
	}
	return interpreter.Recognition{Text: v.Text, Words: []interpreter.Word{{Word: v.Text, End: float64(v.WordEndMS) / 1000}}, PromptVersion: "synthetic-asr-snapshot"}, nil
}
func (b replayBackend) Judge(ctx context.Context, in interpreter.ChunkInput, model string) (interpreter.Result, error) {
	return b.client.Judge(ctx, in, model)
}
func (b replayBackend) Translate(context.Context, interpreter.TranslationInput, string) (interpreter.Result, error) {
	return interpreter.Result{Text: "[decision replay: translation omitted]"}, nil
}

func decisionCommand(parent context.Context, root string, args []string, out io.Writer) error {
	c, e := decision.Load(root)
	if e != nil {
		return e
	}
	if len(args) == 0 || len(args) == 1 && args[0] == "list" {
		for _, v := range c.Cases {
			fmt.Fprintf(out, "%-26s %s\n", v.ID, v.Title)
		}
		return nil
	}
	if len(args) != 2 || args[0] != "run" {
		return fmt.Errorf("Use decision list | decision run CASE")
	}
	v, ok := c.Find(args[1])
	if !ok {
		return fmt.Errorf("Unknown decision case: %s", args[1])
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	client, e := worker.New(ctx, root)
	if e != nil {
		return e
	}
	model, e := client.Model(ctx)
	if e != nil {
		return e
	}
	if model.TranslationOnly {
		return fmt.Errorf("Select a general model for R/W decisions")
	}
	fmt.Fprintf(out, "%s · %s · model=%s\n가상 ASR snapshot으로 실제 통역 제어기와 실제 LLM 판단을 확인합니다. 마이크·STT·번역·TTS는 실행하지 않습니다.\n", v.ID, v.Title, model.Profile)
	var clock atomic.Int64
	s := interpreter.New(ctx, replayBackend{client}, model.Profile, interpreter.Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }, CaptionDebounce: time.Hour, CaptionMaxWait: time.Hour})
	defer s.Close()
	events := []interpreter.Event{}
	requests := 0
	read := func() (interpreter.Event, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event := <-s.Events():
			events = append(events, event)
			if event["type"] == "chunk.request" {
				requests++
			}
			if event["type"] == "recovery" {
				return event, fmt.Errorf("Decision recovery: %v", event)
			}
			return event, nil
		}
	}
	rows := []map[string]any{}
	var mismatch error
	for i, snapshot := range v.Snapshots {
		clock.Store(snapshot.AtMS)
		fmt.Fprintf(out, "\n%5dms %s > %s\n", snapshot.AtMS, v.Track, snapshot.Text)
		before := requests
		data, _ := json.Marshal(snapshot)
		if e = s.Audio(interpreter.Audio{Track: v.Track, WindowID: v.Track + "-1", Sequence: int64(i + 1), Samples: snapshot.Samples, QuietMS: snapshot.QuietMS}, data); e != nil {
			return e
		}
		for {
			event, err := read()
			if err != nil {
				return err
			}
			if event["type"] == "audio.processed" && event["sequence"] == int64(i+1) {
				break
			}
		}
		row := map[string]any{"at_ms": snapshot.AtMS, "source": snapshot.Text, "expected": snapshot.Expected, "actual": "-"}
		rows = append(rows, row)
		if snapshot.Expected == "-" {
			if requests != before {
				mismatch = fmt.Errorf("Unstable or already-waited source invoked the decision model")
				break
			}
			fmt.Fprintln(out, "        대기: 안정화/오디오 근거 확인 또는 이미 W인 원문")
			continue
		}
		var result interpreter.Event
		for {
			event, err := read()
			if err != nil {
				return err
			}
			if event["type"] == "chunk.decision" {
				result = event
				break
			}
		}
		actual, _ := result["label"].(string)
		row["actual"] = actual
		row["latency_ms"] = result["latency_ms"]
		row["prompt_version"] = result["prompt_version"]
		fmt.Fprintf(out, "        LLM=%s · expected=%s · %.1fms\n", actual, snapshot.Expected, result["latency_ms"])
		if actual == "R" {
			for {
				event, err := read()
				if err != nil {
					return err
				}
				if event["type"] == "source.commit" {
					row["committed"] = event["text"]
					fmt.Fprintln(out, "        확정 원문:", event["text"])
					break
				}
			}
		}
		if actual != snapshot.Expected || snapshot.Commit != "" && row["committed"] != snapshot.Commit {
			mismatch = fmt.Errorf("Decision differs from the development case; inspect source and labels before changing either")
			break
		}
	}
	report := map[string]any{"kind": "decision-replay", "case": v.ID, "model": model.Profile, "synthetic_asr": true, "translation_run": false, "rows": rows, "events": events, "matches": mismatch == nil}
	data, _ := json.MarshalIndent(report, "", "  ")
	dir := filepath.Join(root, ".artifacts/decision")
	if e = os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	path := filepath.Join(dir, v.ID+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".json")
	if e = os.WriteFile(path, append(data, '\n'), 0644); e != nil {
		return e
	}
	fmt.Fprintln(out, "\n디버그 기록:", path)
	return mismatch
}
