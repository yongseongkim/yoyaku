package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yoyaku/internal/interpreter"
)

func fixture(t *testing.T, fn http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(fn)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := NewWithProfiles(ctx, map[string]Profile{"gemma26b": {Directory: "gemma"}, "translate12b": {Directory: "translate", TranslationOnly: true}})
	c.LLMURL = server.URL
	c.SpeechURL = server.URL
	c.ControlURL = server.URL
	return c
}
func modelReply(w http.ResponseWriter, id string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "/models/" + id}}})
}
func TestSharedPromptsAndSourceOnlyJudgeRequest(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelReply(w, "gemma")
			return
		}
		var body struct {
			Messages    []struct{ Role, Content string }
			MaxTokens   int `json:"max_tokens"`
			Temperature float64
			Seed        int
			Stream      bool
			Template    map[string]bool `json:"chat_template_kwargs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) != 2 || body.Messages[0].Content != Prompts.Chunk || body.MaxTokens != 4 || body.Temperature != 0 || body.Stream || body.Template["enable_thinking"] {
			t.Error("chunk protocol differs")
		}
		var input map[string]any
		_ = json.Unmarshal([]byte(body.Messages[1].Content), &input)
		if len(input) != 3 || input["text"] != "두 명입니다." || input["language"] != "ko-KR" || input["previous_utterance"] != "何名様でしょうか。" {
			t.Errorf("non-source input: %#v", input)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"R"},"finish_reason":"length"}]}`)
	})
	result, err := c.Judge(t.Context(), interpreter.ChunkInput{Text: "두 명입니다.", Language: "ko-KR", Previous: "何名様でしょうか。"}, "gemma26b")
	if err != nil || result.Text != "R" || result.PromptVersion != "chunk-readiness-v1" {
		t.Fatalf("%#v %v", result, err)
	}
}
func TestTranslationUsesSamePromptAndRejectsTruncation(t *testing.T) {
	var truncate atomic.Bool
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelReply(w, "gemma")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		messages := body["messages"].([]any)
		if messages[0].(map[string]any)["content"] != Prompts.Translations["ko-KR:ja-JP"] || body["max_tokens"] != float64(256) {
			t.Error("translation prompt changed")
		}
		var input map[string]any
		_ = json.Unmarshal([]byte(messages[1].(map[string]any)["content"].(string)), &input)
		if len(input) != 3 {
			t.Error("extra model input")
		}
		finish := "stop"
		if truncate.Load() {
			finish = "length"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "二人です。"}, "finish_reason": finish}}})
	})
	input := interpreter.TranslationInput{Text: "두 명입니다.", Source: "ko-KR", Target: "ja-JP"}
	result, err := c.Translate(t.Context(), input, "gemma26b")
	if err != nil || result.Text != "二人です。" {
		t.Fatal(result, err)
	}
	truncate.Store(true)
	if _, err = c.Translate(t.Context(), input, "gemma26b"); err == nil {
		t.Fatal("truncated translation accepted")
	}
}
func TestChangedModelAndInvalidReadinessRejected(t *testing.T) {
	for _, mode := range []string{"invalid-label", "changed-model", "wrong-expected"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					id := "gemma"
					if mode == "changed-model" && calls.Add(1) > 1 {
						id = "translate"
					}
					modelReply(w, id)
					return
				}
				text := "R"
				if mode == "invalid-label" {
					text = "P"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": text}, "finish_reason": "stop"}}})
			})
			expected := "gemma26b"
			if mode == "wrong-expected" {
				expected = "translate12b"
			}
			if _, err := c.Judge(t.Context(), interpreter.ChunkInput{Text: "네 명입니다.", Language: "ko-KR"}, expected); err == nil {
				t.Fatal("unsafe result accepted")
			}
		})
	}
}
func TestTranslateGemmaNativeAdapterAndClassifierRejection(t *testing.T) {
	var post atomic.Int64
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelReply(w, "translate")
			return
		}
		post.Add(1)
		if r.URL.Path != "/v1/completions" {
			t.Error("native adapter not used")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] != NativePrompt("두 명입니다.", "ko-KR", "ja-JP") || body["messages"] != nil || body["stop"].([]any)[0] != "<end_of_turn>" {
			t.Error("native prompt differs")
		}
		_, _ = io.WriteString(w, `{"choices":[{"text":"二人です。","finish_reason":"stop"}]}`)
	})
	result, err := c.Translate(t.Context(), interpreter.TranslationInput{Text: "두 명입니다.", Source: "ko-KR", Target: "ja-JP"}, "translate12b")
	if err != nil || result.PromptVersion != Prompts.NativeVersion {
		t.Fatal(result, err)
	}
	if _, err = c.Judge(t.Context(), interpreter.ChunkInput{Text: "네 명입니다.", Language: "ko-KR"}, "translate12b"); err == nil || post.Load() != 1 {
		t.Fatal("translation-only model used as judge")
	}
}
func TestHYMTUsesTargetLanguageAndSingleUserTurn(t *testing.T) {
	for _, dir := range []struct{ source, target, name string }{
		{"ko-KR", "ja-JP", "Japanese"}, {"ja-JP", "ko-KR", "Korean"},
	} {
		t.Run(dir.name, func(t *testing.T) {
			var calls int
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					modelReply(w, "hymt")
					return
				}
				calls++
				var body struct {
					Messages []struct{ Role, Content string }
					Prompt   string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				want := "Translate the following segment into " + dir.name + ", without additional explanation.\n\n0350"
				if r.URL.Path != "/v1/chat/completions" || body.Prompt != "" || len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != want {
					t.Errorf("Unexpected HY-MT request: %+v", body)
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"0350"},"finish_reason":"stop"}]}`)
			})
			c.Profiles["hymt18b"] = Profile{Directory: "hymt", TranslationOnly: true, TranslationTemplate: "hy-mt1.5"}
			r, err := c.Translate(t.Context(), interpreter.TranslationInput{Text: "0350", Source: dir.source, Target: dir.target}, "hymt18b")
			if err != nil || r.Text != "0350" || r.PromptVersion != "hy-mt1.5-non-zh-v1" {
				t.Fatal(r, err)
			}
			if _, err = c.Judge(t.Context(), interpreter.ChunkInput{Text: "0350", Language: dir.source}, "hymt18b"); err == nil || calls != 1 {
				t.Fatal("Expected classifier rejection before generation")
			}
		})
	}
}
func TestSpeechLanguageAndTimestampsExplicit(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != "audio-only" || r.Header.Get("X-Source-Language") != "ja" || r.Header.Get("X-Word-Timestamps") != "1" {
			t.Error("speech route changed")
		}
		_, _ = io.WriteString(w, `{"text":"何名様でしょうか。","words":[{"word":"何名様でしょうか。","start":0,"end":1}],"prompt_version":"reservation-asr-v1"}`)
	})
	r, err := c.Transcribe(t.Context(), []byte("audio-only"), "ja", true)
	if err != nil || r.Text != "何名様でしょうか。" || len(r.Words) != 1 {
		t.Fatal(r, err)
	}
}
func TestInferenceQueueSerialBoundedAndCancelledPendingSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newQueue(ctx, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 2)
	var calls atomic.Int64
	go func() {
		_, err := q.do(ctx, func(context.Context) (any, error) { calls.Add(1); close(entered); <-release; return nil, nil })
		done <- err
	}()
	<-entered
	pending, stop := context.WithCancel(ctx)
	go func() {
		_, err := q.do(pending, func(context.Context) (any, error) { calls.Add(1); return nil, nil })
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(q.jobs) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := q.do(ctx, func(context.Context) (any, error) { return nil, nil }); err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatal("unbounded inference")
	}
	stop()
	close(release)
	<-done
	<-done
	// 첫 요청의 결과를 받기 전에 대기 중인 요청을 취소해도 모델은 첫 요청만 처리해야 한다.
	if calls.Load() != 1 {
		t.Fatal("cancelled pending work reached model")
	}
}
