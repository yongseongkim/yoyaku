package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"yoyaku/internal/interpreter"
	"yoyaku/internal/model"
	"yoyaku/internal/research"
	"yoyaku/internal/worker"
)

func verify(root string, out io.Writer) error { return research.Verify(root, out) }
func check(ctx context.Context, root string, out io.Writer) error {
	client, err := worker.New(ctx, root)
	if err != nil {
		return err
	}
	identity, err := client.Model(ctx)
	if err != nil {
		return err
	}
	type row struct {
		Task, Source, Expected, Output string
		LatencyMS                      float64
		Error                          string `json:",omitempty"`
	}
	rows := []row{}
	failed := false
	for _, c := range []struct{ text, language, label string }{{"두 명 예약하고", "ko-KR", "W"}, {"두 명 예약하고 싶습니다.", "ko-KR", "R"}, {"何名様でしょうか。", "ja-JP", "R"}, {"19時が無理なら", "ja-JP", "W"}} {
		result, e := client.Judge(ctx, interpreter.ChunkInput{Text: c.text, Language: c.language}, identity.Profile)
		r := row{Task: "readiness", Source: c.text, Expected: c.label, Output: result.Text, LatencyMS: result.LatencyMS}
		if e != nil {
			r.Error = e.Error()
		}
		if e != nil || result.Text != c.label {
			failed = true
		}
		rows = append(rows, r)
	}
	for _, c := range []struct{ text, source, target string }{{"두 명 예약하고 싶습니다.", "ko-KR", "ja-JP"}, {"何名様でしょうか。", "ja-JP", "ko-KR"}, {"まだ予約は確定していません。", "ja-JP", "ko-KR"}, {"7시가 아니라 7시 반입니다.", "ko-KR", "ja-JP"}} {
		result, e := client.Translate(ctx, interpreter.TranslationInput{Text: c.text, Source: c.source, Target: c.target}, identity.Profile)
		r := row{Task: "translation", Source: c.text, Output: result.Text, LatencyMS: result.LatencyMS}
		if e != nil {
			r.Error = e.Error()
			failed = true
		}
		rows = append(rows, r)
	}
	catalog, e := model.Read(root)
	if e != nil {
		return e
	}
	report := map[string]any{"protocol": "go-native-smoke-v1", "at": time.Now().UTC(), "profile": identity.Profile, "model": catalog.Profiles[identity.Profile], "rows": rows, "translation_quality": "Outputs require human semantic review; successful generation is not translation accuracy."}
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Fprintln(out, string(data))
	dir := filepath.Join(root, ".artifacts/native-check")
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("Readiness mismatch or inference failure; inspect %s", strings.TrimPrefix(dir, root+"/"))
	}
	return nil
}
