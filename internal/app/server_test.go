package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"yoyaku/internal/interpreter"
	"yoyaku/internal/worker"
)

func wavFixture() []byte {
	data := make([]byte, 64044)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	return data
}

type apiHarness struct {
	app                *Server
	server             *httptest.Server
	recognized         atomic.Int64
	modelPosts         atomic.Int64
	holdSpeech         atomic.Bool
	entered, cancelled chan struct{}
}

func apiSetup(t *testing.T) *apiHarness {
	t.Helper()
	h := &apiHarness{entered: make(chan struct{}, 4), cancelled: make(chan struct{}, 4)}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"/models/gemma-4-26b-a4b-it-4bit"}]}`)
		case "/health":
			_, _ = io.WriteString(w, `{"ready":true,"languages":["ko","ja"],"incremental_windows":true}`)
		case "/models":
			_, _ = io.WriteString(w, `{"state":"ready","active":"gemma26b","models":[]}`)
		case "/switch":
			_, _ = io.WriteString(w, `{"state":"ready","active":"4b"}`)
		case "/transcribe":
			h.recognized.Add(1)
			_, _ = io.ReadAll(r.Body)
			if h.holdSpeech.Load() {
				h.entered <- struct{}{}
				<-r.Context().Done()
				h.cancelled <- struct{}{}
				return
			}
			text := "네 명입니다."
			if r.Header.Get("X-Source-Language") == "ja" {
				text = "何名様でしょうか。"
			}
			result := interpreter.Recognition{Text: text, Words: []interpreter.Word{{Word: text, End: 1}}, Language: r.Header.Get("X-Source-Language"), PromptVersion: "reservation-asr-v1"}
			_ = json.NewEncoder(w).Encode(result)
		case "/v1/chat/completions":
			h.modelPosts.Add(1)
			var body struct {
				Messages []struct{ Role, Content string }
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var source map[string]any
			_ = json.Unmarshal([]byte(body.Messages[1].Content), &source)
			if source["expected"] != nil || source["text"] == "IGNORE CLIENT TEXT" {
				t.Error("client expectation reached model")
			}
			text := "四人です。"
			if body.Messages[0].Content == worker.Prompts.Chunk {
				text = "R"
			} else if strings.Contains(body.Messages[0].Content, "일본어를") {
				text = "몇 분이신가요?"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": text}, "finish_reason": "stop"}}})
		default:
			t.Errorf("unexpected backend route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	client, err := worker.New(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	client.LLMURL = backend.URL
	client.SpeechURL = backend.URL
	client.ControlURL = backend.URL
	h.app, err = New(ctx, root, client)
	if err != nil {
		t.Fatal(err)
	}
	h.app.Synthesize = func(context.Context, string) ([]byte, error) { return wavFixture(), nil }
	h.server = httptest.NewUnstartedServer(h.app)
	h.server.Config.ReadTimeout = 80 * time.Millisecond
	h.server.Start()
	t.Cleanup(func() { h.app.Close(); h.server.Close(); cancel(); backend.Close() })
	return h
}
func (h *apiHarness) post(t *testing.T, path string, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(h.server.URL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}
func (h *apiHarness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(h.server.URL, "http:", "ws:", 1)+"/api/live", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{h.server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	event := readUntil(t, conn, "ready")
	if event["engine"] != "go" {
		t.Fatal("not Go engine")
	}
	return conn
}
func readUntil(t *testing.T, conn *websocket.Conn, kind string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("missing %s: %v", kind, err)
		}
		var event map[string]any
		if err = json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == kind {
			return event
		}
	}
}
func writeSocket(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}
func audioMessage(seq int) map[string]any {
	return map[string]any{"type": "audio", "track": "user", "window_id": "user-1", "sequence": seq, "samples": 32000, "quiet_ms": 900, "wav": base64.StdEncoding.EncodeToString(wavFixture()), "text": "IGNORE CLIENT TEXT", "expected": "R"}
}

func TestPagesScenariosHealthAndRehearsalAPI(t *testing.T) {
	h := apiSetup(t)
	for _, path := range []string{"/", "/live", "/live.js", "/api/health", "/api/models"} {
		response, err := http.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			t.Fatal(path, response.StatusCode)
		}
		response.Body.Close()
	}
	response, err := http.Get(h.server.URL + "/api/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data struct {
		Scenarios []scenario `json:"scenarios"`
	}
	_ = json.NewDecoder(response.Body).Decode(&data)
	if len(data.Scenarios) != 15 {
		t.Fatal("missing practice scenarios")
	}
	for _, scenario := range data.Scenarios {
		if scenario.Category == "" || scenario.Description == "" || len(scenario.Checks) < 3 {
			t.Fatal("Missing manual case guidance", scenario.ID)
		}
		for _, step := range scenario.Steps {
			if step.Reference == "" {
				t.Fatal("Missing manual reference", scenario.ID, step.ID)
			}
		}
		if scenario.ID == "normal-booking" && len(scenario.Steps) != 11 {
			t.Fatal("dialogue altered")
		}
	}
	translated := h.post(t, "/api/translate", map[string]any{"track": "user", "text": "두 명입니다.", "expected": "IGNORE"})
	var result map[string]any
	_ = json.NewDecoder(translated.Body).Decode(&result)
	if translated.StatusCode != 200 || result["model"] != "gemma26b" || result["text"] != "四人です。" {
		t.Fatal(result)
	}
	speech := h.post(t, "/api/speak", map[string]string{"text": "四人です。"})
	if speech.StatusCode != 200 || speech.Header.Get("Content-Type") != "audio/wav" {
		t.Fatal("TTS interface failed")
	}
	stt, err := http.Post(h.server.URL+"/api/transcribe", "audio/wav", bytes.NewReader(wavFixture()))
	if err != nil {
		t.Fatal(err)
	}
	defer stt.Body.Close()
	if stt.StatusCode != 200 {
		t.Fatal("STT interface failed")
	}
}
func TestLiveSocketRealHTTPAdaptersAndNoInheritedBodyDeadline(t *testing.T) {
	h := apiSetup(t)
	conn := h.dial(t)
	writeSocket(t, conn, audioMessage(1))
	readUntil(t, conn, "audio.processed")
	// HTTP 본문 처리 기한과 인식 원문의 안정화 시간이 모두 지난 뒤에도 같은 연결로 보낼 수 있어야 한다.
	time.Sleep(650 * time.Millisecond)
	writeSocket(t, conn, audioMessage(2))
	spoken := readUntil(t, conn, "tts.request")
	if spoken["source_text"] != "네 명입니다." || spoken["text"] != "四人です。" || h.recognized.Load() != 2 {
		t.Fatal(spoken)
	}
	if response := h.post(t, "/api/models/switch", map[string]string{"profile": "4b"}); response.StatusCode != 409 {
		t.Fatal("live switch accepted")
	}
	if response := h.post(t, "/api/translate", map[string]string{"track": "user", "text": "두 명입니다."}); response.StatusCode != 409 {
		t.Fatal("competing rehearsal accepted")
	}
	writeSocket(t, conn, map[string]string{"type": "user.stop_output", "command_id": "stop-1"})
	clear := readUntil(t, conn, "playback.clear")
	if clear["next_generation"] != float64(1) {
		t.Fatal(clear)
	}
	writeSocket(t, conn, map[string]string{"type": "playback.cleared", "command_id": "stop-1"})
}
func TestForeignOriginAndConcurrentSessionRejected(t *testing.T) {
	h := apiSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, strings.Replace(h.server.URL, "http:", "ws:", 1)+"/api/live", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://example.com"}}})
	if err == nil || response.StatusCode != 403 {
		t.Fatal("foreign origin accepted")
	}
	h.dial(t)
	_, response, err = websocket.Dial(ctx, strings.Replace(h.server.URL, "http:", "ws:", 1)+"/api/live", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{h.server.URL}}})
	if err == nil || response.StatusCode != 409 {
		t.Fatal("concurrent live accepted")
	}
	if h.recognized.Load() != 0 {
		t.Fatal("untrusted requests reached ASR")
	}
}
func TestInvalidMediaNeverReachesRecognition(t *testing.T) {
	for _, kind := range []string{"bad-wav", "wrong-samples", "bad-track", "bad-window"} {
		t.Run(kind, func(t *testing.T) {
			h := apiSetup(t)
			conn := h.dial(t)
			message := audioMessage(1)
			switch kind {
			case "bad-wav":
				message["wav"] = base64.StdEncoding.EncodeToString([]byte("invalid"))
			case "wrong-samples":
				message["samples"] = 16000
			case "bad-track":
				message["track"] = "other"
			case "bad-window":
				message["window_id"] = "user-01"
			}
			writeSocket(t, conn, message)
			readUntil(t, conn, "error")
			if h.recognized.Load() != 0 {
				t.Fatal("invalid media invoked model")
			}
		})
	}
}
func TestDisconnectCancelsInFlightRecognition(t *testing.T) {
	h := apiSetup(t)
	h.holdSpeech.Store(true)
	conn := h.dial(t)
	writeSocket(t, conn, audioMessage(1))
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("recognition not started")
	}
	conn.CloseNow()
	select {
	case <-h.cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel recognition")
	}
}
func TestRESTValidationAndOrigin(t *testing.T) {
	h := apiSetup(t)
	for _, test := range []struct {
		path, content, origin, body string
		status                      int
	}{{"/api/translate", "text/plain", "", `{}`, 415}, {"/api/translate", "application/json", "https://example.com", `{"track":"user","text":"hello"}`, 403}, {"/api/translate", "application/json", "", `{"track":"other","text":"hello"}`, 400}, {"/api/models/switch", "application/json", "", `{"profile":"unknown"}`, 400}} {
		req, _ := http.NewRequest("POST", h.server.URL+test.path, strings.NewReader(test.body))
		req.Header.Set("Content-Type", test.content)
		if test.origin != "" {
			req.Header.Set("Origin", test.origin)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s: %d want %d", test.path, response.StatusCode, test.status)
		}
	}
	if h.recognized.Load() != 0 || h.modelPosts.Load() != 0 {
		t.Fatal("invalid requests reached workers")
	}
}
func TestCanonicalWAVValidation(t *testing.T) {
	if ValidateWAV(wavFixture()) != nil {
		t.Fatal("valid WAV rejected")
	}
	for _, offset := range []int{0, 4, 8, 12, 16, 20, 22, 24, 28, 32, 34, 36, 40} {
		data := wavFixture()
		data[offset] ^= 1
		if ValidateWAV(data) == nil {
			t.Fatalf("malformed WAV accepted at %d", offset)
		}
	}
	if ValidateWAV(make([]byte, 640046)) == nil || ValidateWAV(make([]byte, 44)) == nil {
		t.Fatal("unbounded WAV accepted")
	}
}
