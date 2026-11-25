package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"yoyaku/internal/interpreter"
	"yoyaku/internal/model"
	"yoyaku/internal/worker"
)

// benchmark는 사용자가 직접 실행할 때만 새 측정 결과를 만든다.
func benchmark(ctx context.Context, root string, args []string, out io.Writer) error {
	chunkFile, translationFile := "readiness.json", "probes.json"
	suiteName := "baseline"
	if len(args) == 1 && args[0] == "disfluency" {
		suiteName = args[0]
		chunkFile, translationFile = "disfluency.json", "reservation-intent.json"
	} else if len(args) != 0 {
		return fmt.Errorf("use: yoyaku benchmark [disfluency]")
	}
	var chunks struct {
		Cases []struct {
			ID, Text, Language, Expected string
			Previous                     string `json:"previous_utterance"`
		}
	}
	var translations struct {
		Cases []struct {
			ID, Text string
			Source   string `json:"source_language"`
			Target   string `json:"target_language"`
		}
	}
	chunkData, err := os.ReadFile(filepath.Join(root, "testdata/decision", chunkFile))
	if err != nil {
		return err
	}
	translationData, err := os.ReadFile(filepath.Join(root, "testdata/translation", translationFile))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(chunkData, &chunks); err != nil {
		return err
	}
	if err = json.Unmarshal(translationData, &translations); err != nil {
		return err
	}
	client, err := worker.New(ctx, root)
	if err != nil {
		return err
	}
	identity, err := client.Model(ctx)
	if err != nil {
		return err
	}
	catalog, err := model.Read(root)
	if err != nil {
		return err
	}
	pins, err := model.ReadRuntimePins(root)
	if err != nil {
		return err
	}
	rows := []map[string]any{}
	matches, premature, waiting, invalid, ambiguous := 0, 0, 0, 0, 0
	if !identity.TranslationOnly {
		for _, c := range chunks.Cases {
			if err := ctx.Err(); err != nil {
				return err
			}
			source := interpreter.ChunkInput{Text: c.Text, Language: c.Language, Previous: c.Previous}
			result, e := client.Judge(ctx, source, identity.Profile)
			row := map[string]any{"id": c.ID, "task": "chunk_readiness", "source": source, "expected": c.Expected, "text": result.Text, "latency_ms": result.LatencyMS, "prompt_version": result.PromptVersion, "status": "ok"}
			if e != nil {
				row["error"] = e.Error()
				row["status"] = "error"
				invalid++
			} else if c.Expected == "?" {
				ambiguous++
			} else if result.Text == c.Expected {
				matches++
			} else if result.Text == "R" {
				premature++
			} else {
				waiting++
			}
			rows = append(rows, row)
			fmt.Fprintf(out, "chunk %-32s %s (expected %s)\n", c.ID, result.Text, c.Expected)
		}
	}
	delivered := 0
	for _, c := range translations.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := interpreter.TranslationInput{Text: c.Text, Source: c.Source, Target: c.Target}
		result, e := client.Translate(ctx, source, identity.Profile)
		row := map[string]any{"id": c.ID, "task": "translation", "source": source, "text": result.Text, "latency_ms": result.LatencyMS, "prompt_version": result.PromptVersion, "status": "ok"}
		if e != nil {
			row["error"] = e.Error()
			row["status"] = "error"
		} else {
			delivered++
		}
		rows = append(rows, row)
		fmt.Fprintf(out, "translation %-32s %s\n", c.ID, result.Text)
	}
	suite := sha256.Sum256(append(append(append([]byte{}, chunkData...), translationData...), workerPromptBytes()...))
	report := map[string]any{"protocol": "go-native-text-evaluation-v1", "suite": suiteName, "profile": identity.Profile, "model": catalog.Profiles[identity.Profile], "runtimes": pins, "at": time.Now().UTC(), "suite_hash": hex.EncodeToString(suite[:]), "execution_complete": len(rows) == len(translations.Cases)+func() int {
		if identity.TranslationOnly {
			return 0
		}
		return len(chunks.Cases)
	}(), "summary": map[string]any{"matches": matches, "premature_ready": premature, "unnecessary_wait": waiting, "invalid_or_error": invalid, "ambiguous_observed": ambiguous, "translations_delivered": delivered, "translation_quality": "pending semantic review; delivery is not accuracy"}, "rows": rows}
	data, _ := json.MarshalIndent(report, "", "  ")
	base := filepath.Join(root, ".artifacts/native-benchmark")
	if err = os.MkdirAll(base, 0755); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return err
	}
	path := filepath.Join(directory, identity.Profile+".json")
	if err = os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return err
	}
	fmt.Fprintln(out, "Saved new native-runtime cohort:", path)
	return nil
}
func workerPromptBytes() []byte { b, _ := json.Marshal(worker.Prompts); return b }
