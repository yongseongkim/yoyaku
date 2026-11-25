package worker

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"yoyaku/internal/decision"
	"yoyaku/internal/interpreter"
	"yoyaku/internal/model"
)

//go:embed prompts.json
var promptBytes []byte
var Prompts struct {
	ChunkVersion       string            `json:"chunk_version"`
	Chunk              string            `json:"chunk_prompt"`
	TranslationVersion string            `json:"translation_version"`
	NativeVersion      string            `json:"native_version"`
	Translations       map[string]string `json:"translations"`
}

func init() {
	if err := json.Unmarshal(promptBytes, &Prompts); err != nil {
		panic(err)
	}
}

type Profile = model.Profile
type Model struct {
	Profile         string `json:"profile"`
	TranslationOnly bool   `json:"translation_only"`
}
type Client struct {
	HTTP                          *http.Client
	LLMURL, SpeechURL, ControlURL string
	Profiles                      map[string]Profile
	llm, stt                      *queue
	key                           string
}

func New(ctx context.Context, root string) (*Client, error) {
	data, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Profiles map[string]Profile `json:"profiles"`
	}
	if err = json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	key, err := model.LocalKey(root)
	if err != nil {
		return nil, err
	}
	client := NewWithProfiles(ctx, catalog.Profiles)
	client.key = key
	return client, nil
}
func NewWithProfiles(ctx context.Context, profiles map[string]Profile) *Client {
	return &Client{HTTP: &http.Client{Timeout: 135 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, LLMURL: "http://127.0.0.1:8091", SpeechURL: "http://127.0.0.1:8092", ControlURL: "http://127.0.0.1:8093", Profiles: profiles, llm: newQueue(ctx, 8), stt: newQueue(ctx, 4)}
}
func marshal(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
func (c *Client) request(ctx context.Context, method, url string, body []byte, headers map[string]string, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if c.key != "" && strings.HasPrefix(url, c.LLMURL+"/") {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 2*1024*1024 {
		return errors.New("Oversized worker response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Local worker HTTP %d", response.StatusCode)
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}
func (c *Client) Model(ctx context.Context) (Model, error) {
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.request(ctx, "GET", c.LLMURL+"/v1/models", nil, nil, &data); err != nil {
		return Model{}, err
	}
	if len(data.Data) != 1 {
		return Model{}, errors.New("Run one registered local model")
	}
	for id, p := range c.Profiles {
		if strings.HasSuffix(data.Data[0].ID, "/"+p.Directory) {
			return Model{id, p.TranslationOnly}, nil
		}
	}
	return Model{}, errors.New("Unregistered local model")
}
func (c *Client) SpeechHealth(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := c.request(ctx, "GET", c.SpeechURL+"/health", nil, nil, &result)
	return result, err
}
func (c *Client) Control(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.request(ctx, method, c.ControlURL+path, body, map[string]string{"Content-Type": "application/json"}, &result)
	return result, err
}
func (c *Client) Transcribe(ctx context.Context, wav []byte, language string, timestamps bool) (interpreter.Recognition, error) {
	value, err := c.stt.do(ctx, func(ctx context.Context) (any, error) {
		var result interpreter.Recognition
		flag := "0"
		if timestamps {
			flag = "1"
		}
		err := c.request(ctx, "POST", c.SpeechURL+"/transcribe", wav, map[string]string{"Content-Type": "audio/wav", "X-Source-Language": language, "X-Word-Timestamps": flag}, &result)
		return result, err
	})
	if err != nil {
		return interpreter.Recognition{}, err
	}
	return value.(interpreter.Recognition), nil
}
func (c *Client) Judge(ctx context.Context, input interpreter.ChunkInput, expected string) (interpreter.Result, error) {
	if strings.TrimSpace(input.Text) == "" || utf8.RuneCountInString(input.Text) > 4000 {
		return interpreter.Result{}, errors.New("Invalid chunk source")
	}
	previous := []rune(input.Previous)
	if len(previous) > 1000 {
		input.Previous = string(previous[len(previous)-1000:])
	}
	return c.infer(ctx, input, Prompts.Chunk, 4, expected, true, Prompts.ChunkVersion)
}
func (c *Client) Translate(ctx context.Context, input interpreter.TranslationInput, expected string) (interpreter.Result, error) {
	prompt, ok := Prompts.Translations[input.Source+":"+input.Target]
	if !ok || strings.TrimSpace(input.Text) == "" || utf8.RuneCountInString(input.Text) > 8192 {
		return interpreter.Result{}, errors.New("Invalid translation source")
	}
	return c.infer(ctx, input, prompt, 256, expected, false, Prompts.TranslationVersion)
}
func NativePrompt(text, source, target string) string {
	names := map[string][2]string{"ja-JP": {"Japanese", "ja"}, "ko-KR": {"Korean", "ko"}}
	from, to := names[source], names[target]
	return fmt.Sprintf("<start_of_turn>user\nYou are a professional %s (%s) to %s (%s) translator. Your goal is to accurately convey the meaning and nuances of the original %s text while adhering to %s grammar, vocabulary, and cultural sensitivities.\nProduce only the %s translation, without any additional explanations or commentary. Please translate the following %s text into %s:\n\n\n%s<end_of_turn>\n<start_of_turn>model\n", from[0], from[1], to[0], to[1], from[0], to[0], to[0], from[0], to[0], strings.TrimSpace(text))
}
func (c *Client) infer(ctx context.Context, input any, prompt string, maxTokens int, expected string, judge bool, version string) (interpreter.Result, error) {
	value, err := c.llm.do(ctx, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		started := time.Now()
		model, err := c.Model(ctx)
		if err != nil {
			return nil, err
		}
		if expected != "" && model.Profile != expected {
			return nil, errors.New("Model changed; restart interpretation")
		}
		if judge && model.TranslationOnly {
			return nil, errors.New("Selected model supports translation only")
		}
		body := map[string]any{"model": "default_model", "stream": false, "temperature": 0, "seed": 0, "max_tokens": maxTokens, "chat_template_kwargs": map[string]bool{"enable_thinking": false}}
		path := "/v1/chat/completions"
		switch {
		case c.Profiles[model.Profile].TranslationTemplate == "hy-mt1.5":
			translation := input.(interpreter.TranslationInput)
			target := map[string]string{"ja-JP": "Japanese", "ko-KR": "Korean"}[translation.Target]
			content := fmt.Sprintf("Translate the following segment into %s, without additional explanation.\n\n%s", target, translation.Text)
			body["messages"] = []map[string]string{{"role": "user", "content": content}}
			version = "hy-mt1.5-non-zh-v1"
		case model.TranslationOnly:
			translation := input.(interpreter.TranslationInput)
			body["prompt"] = NativePrompt(translation.Text, translation.Source, translation.Target)
			version = Prompts.NativeVersion
			body["stop"] = []string{"<end_of_turn>"}
			path = "/v1/completions"
		default:
			data, err := marshal(input)
			if err != nil {
				return nil, err
			}
			body["messages"] = []map[string]string{{"role": "system", "content": prompt}, {"role": "user", "content": string(data)}}
		}
		data, err := marshal(body)
		if err != nil {
			return nil, err
		}
		var response struct {
			Choices []struct {
				Text    string `json:"text"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Finish string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err = c.request(ctx, "POST", c.LLMURL+path, data, map[string]string{"Content-Type": "application/json"}, &response); err != nil {
			return nil, err
		}
		if len(response.Choices) != 1 {
			return nil, errors.New("Missing model response")
		}
		choice := response.Choices[0]
		text := choice.Message.Content
		if path == "/v1/completions" {
			text = choice.Text
		}
		text = strings.TrimSpace(text)
		if judge {
			if _, err := decision.ParseLabel(text); err != nil {
				return nil, err
			}
		}
		if text == "" || len(text) > 16000 || (!judge && choice.Finish == "length") || (judge && text != "R" && text != "W") {
			return nil, errors.New("Invalid or truncated model response")
		}
		after, err := c.Model(ctx)
		if err != nil || after.Profile != model.Profile {
			return nil, errors.New("Model changed during inference")
		}
		return interpreter.Result{Text: text, PromptVersion: version, LatencyMS: float64(time.Since(started).Microseconds()) / 1000}, nil
	})
	if err != nil {
		return interpreter.Result{}, err
	}
	return value.(interpreter.Result), nil
}
