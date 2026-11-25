// app 패키지는 브라우저에서 쓰는 예약 연습 화면과 실시간 통역 API를 제공한다.
package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"yoyaku/internal/interpreter"
	reservation "yoyaku/internal/scenario"
	"yoyaku/internal/worker"
)

type Server struct {
	ctx                          context.Context
	cancel                       context.CancelFunc
	root                         string
	client                       *worker.Client
	scenarios                    []scenario
	mu                           sync.Mutex
	active                       int
	upgrading, switching, closed bool
	upgradeSequence              uint64
	live                         *interpreter.Session
	conn                         *websocket.Conn
	Synthesize                   func(context.Context, string) ([]byte, error)
}
type step struct {
	ID        string  `json:"id"`
	Speaker   string  `json:"speaker"`
	Text      string  `json:"text,omitempty"`
	Example   string  `json:"example,omitempty"`
	After     *string `json:"after,omitempty"`
	Reference string  `json:"reference,omitempty"`
}
type scenario struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Checks      []string `json:"checks"`
	Steps       []step   `json:"steps"`
}
type Health struct {
	Translation bool     `json:"translation"`
	Model       *string  `json:"model"`
	Speech      bool     `json:"speech"`
	Languages   []string `json:"languages"`
	Live        bool     `json:"live"`
	Local       bool     `json:"local"`
	Engine      string   `json:"engine"`
}

func New(ctx context.Context, root string, client *worker.Client) (*Server, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{ctx: ctx, cancel: cancel, root: root, client: client, Synthesize: synthesize}
	fixtures, err := reservation.Load(root)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, f := range fixtures.Scenarios {
		p := scenario{ID: f.ID, Title: f.Title, Category: f.Category, Description: f.Description, Checks: f.Checks}
		for i, t := range f.Turns {
			item := step{ID: t.ID, Speaker: "shop", Text: t.Text, Reference: t.KO}
			if t.Speaker == "B" {
				item = step{ID: t.ID, Speaker: "user", Example: t.KO, Reference: t.Japanese}
				if i > 0 {
					previous := f.Turns[i-1].ID
					item.After = &previous
				}
			}
			p.Steps = append(p.Steps, item)
		}
		s.scenarios = append(s.scenarios, p)
	}
	return s, nil
}

var assets = map[string]string{"/": "demo.html", "/demo.js": "demo.js", "/demo.css": "demo.css", "/voice-recorder.js": "voice-recorder.js", "/recorder-worklet.js": "recorder-worklet.js", "/favicon.svg": "favicon.svg", "/live": "live.html", "/live.js": "live.js", "/continuous-audio.js": "continuous-audio.js", "/speech-queue.js": "speech-queue.js"}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host
}
func readJSON(w http.ResponseWriter, r *http.Request, max int64, value any) error {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return errors.New("JSON 요청이 필요합니다")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/live" {
		s.socket(w, r)
		return
	}
	if file, ok := assets[r.URL.Path]; ok && r.Method == http.MethodGet {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, filepath.Join(s.root, "public", file))
		return
	}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/api/health":
			reply(w, 200, s.Health(r.Context()))
			return
		case "/api/scenarios":
			reply(w, 200, map[string]any{"scenarios": s.scenarios, "mode": "voice-rehearsal", "scripted_partner": true})
			return
		case "/api/models":
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			data, err := s.client.Control(ctx, "GET", "/models", nil)
			if err != nil {
				failure(w, 503, err.Error())
			} else {
				reply(w, 200, data)
			}
			return
		}
	}
	if r.Method != "POST" {
		failure(w, 404, "요청을 찾을 수 없습니다.")
		return
	}
	if !sameOrigin(r) {
		failure(w, 403, "같은 화면에서 요청해 주세요.")
		return
	}
	if r.URL.Path == "/api/models/switch" {
		s.switchModel(w, r)
		return
	}
	if r.URL.Path != "/api/transcribe" && r.URL.Path != "/api/translate" && r.URL.Path != "/api/speak" {
		failure(w, 404, "요청을 찾을 수 없습니다.")
		return
	}
	s.mu.Lock()
	if s.closed || s.switching {
		s.mu.Unlock()
		failure(w, 503, "서비스가 종료되거나 모델을 불러오고 있습니다.")
		return
	}
	if (s.live != nil || s.upgrading) && r.URL.Path != "/api/speak" {
		s.mu.Unlock()
		failure(w, 409, "연속 통역을 종료한 뒤 문장 연습을 사용해 주세요.")
		return
	}
	if s.active >= 2 {
		s.mu.Unlock()
		failure(w, 429, "앞선 요청이 끝난 후 다시 시도해 주세요.")
		return
	}
	s.active++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if r.URL.Path == "/api/transcribe" {
		if r.Header.Get("Content-Type") != "audio/wav" {
			failure(w, 415, "WAV 녹음이 필요합니다.")
			return
		}
		wav, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 640044))
		if err != nil {
			failure(w, 413, "20초 이하로 녹음해 주세요.")
			return
		}
		if err = ValidateWAV(wav); err != nil {
			failure(w, 400, err.Error())
			return
		}
		result, err := s.client.Transcribe(ctx, wav, "ko", false)
		if err != nil {
			failure(w, 503, err.Error())
		} else {
			reply(w, 200, result)
		}
		return
	}
	var body struct {
		Text  string `json:"text"`
		Track string `json:"track"`
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		failure(w, 415, "JSON 요청이 필요합니다.")
		return
	}
	if err := readJSON(w, r, 32768, &body); err != nil {
		failure(w, 400, err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" || utf8.RuneCountInString(body.Text) > 2000 {
		failure(w, 400, "1~2,000자의 문장이 필요합니다.")
		return
	}
	if r.URL.Path == "/api/speak" {
		data, err := s.Synthesize(ctx, body.Text)
		if err != nil {
			failure(w, 503, "일본어 음성 합성에 실패했습니다.")
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = w.Write(data)
		return
	}
	if body.Track != "user" && body.Track != "shop" {
		failure(w, 400, "점원 또는 사용자 방향을 지정해 주세요.")
		return
	}
	input := interpreter.TranslationInput{Text: body.Text, Source: "ko-KR", Target: "ja-JP"}
	if body.Track == "shop" {
		input.Source, input.Target = "ja-JP", "ko-KR"
	}
	model, err := s.client.Model(ctx)
	if err != nil {
		failure(w, 503, err.Error())
		return
	}
	result, err := s.client.Translate(ctx, input, model.Profile)
	if err != nil {
		failure(w, 503, err.Error())
		return
	}
	reply(w, 200, map[string]any{"text": result.Text, "source_text": body.Text, "track": body.Track, "latency_ms": result.LatencyMS, "prompt_version": result.PromptVersion, "model": model.Profile})
}
func (s *Server) Health(parent context.Context) Health {
	ctx, cancel := context.WithTimeout(parent, 1500*time.Millisecond)
	defer cancel()
	type modelResult struct {
		model worker.Model
		err   error
	}
	type speechResult struct {
		data map[string]any
		err  error
	}
	mc := make(chan modelResult, 1)
	sc := make(chan speechResult, 1)
	go func() { m, e := s.client.Model(ctx); mc <- modelResult{m, e} }()
	go func() { d, e := s.client.SpeechHealth(ctx); sc <- speechResult{d, e} }()
	m, sp := <-mc, <-sc
	h := Health{Translation: m.err == nil, Speech: sp.err == nil, Local: true, Engine: "go", Languages: []string{"ko"}}
	if m.err == nil {
		h.Model = &m.model.Profile
	}
	ja := false
	if langs, ok := sp.data["languages"].([]any); ok {
		h.Languages = []string{}
		for _, item := range langs {
			if text, ok := item.(string); ok {
				h.Languages = append(h.Languages, text)
				if text == "ja" {
					ja = true
				}
			}
		}
	}
	h.Live = h.Translation && !m.model.TranslationOnly && h.Speech && sp.data["incremental_windows"] == true && ja
	return h
}
func (s *Server) switchModel(w http.ResponseWriter, r *http.Request) {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		failure(w, 415, "JSON 요청이 필요합니다.")
		return
	}
	var body struct {
		Profile string `json:"profile"`
	}
	if err := readJSON(w, r, 2048, &body); err != nil {
		failure(w, 400, err.Error())
		return
	}
	if _, ok := s.client.Profiles[body.Profile]; !ok {
		failure(w, 400, "등록된 모델을 선택해 주세요.")
		return
	}
	s.mu.Lock()
	if s.closed || s.live != nil || s.upgrading || s.switching || s.active > 0 {
		s.mu.Unlock()
		failure(w, 409, "진행 중인 작업이나 연속 통역을 종료한 후 모델을 바꿔 주세요.")
		return
	}
	s.switching = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.switching = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 135*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	data, _ := json.Marshal(body)
	result, err := s.client.Control(ctx, "POST", "/switch", data)
	if err != nil {
		failure(w, 503, err.Error())
	} else {
		reply(w, 200, result)
	}
}
func ValidateWAV(wav []byte) error {
	bad := func() error { return errors.New("0.3~20초의 16kHz 모노 PCM16 WAV가 필요합니다.") }
	if len(wav) < 9644 || len(wav) > 640044 || (len(wav)-44)%2 != 0 {
		return bad()
	}
	u16 := func(n int) uint16 { return binary.LittleEndian.Uint16(wav[n:]) }
	u32 := func(n int) uint32 { return binary.LittleEndian.Uint32(wav[n:]) }
	if string(wav[:4]) != "RIFF" || u32(4) != uint32(len(wav)-8) || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " || u32(16) != 16 || u16(20) != 1 || u16(22) != 1 || u32(24) != 16000 || u32(28) != 32000 || u16(32) != 2 || u16(34) != 16 || string(wav[36:40]) != "data" || u32(40) != uint32(len(wav)-44) {
		return bad()
	}
	return nil
}
func (s *Server) socket(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.Header.Get("Origin") != "http://"+r.Host {
		failure(w, 403, "Use the same-origin live page")
		return
	}
	s.mu.Lock()
	if s.closed || s.live != nil || s.upgrading || s.switching || s.active > 0 {
		s.mu.Unlock()
		failure(w, 409, "Another media session is active")
		return
	}
	s.upgrading = true
	s.upgradeSequence++
	claim := s.upgradeSequence
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.upgradeSequence == claim {
			s.upgrading = false
		}
		s.mu.Unlock()
	}()
	h := s.Health(r.Context())
	if !h.Live || h.Model == nil {
		failure(w, 503, "Start a general LLM and Korean/Japanese speech worker")
		return
	}
	// 통역은 최초 HTTP 요청보다 오래 이어질 수 있어 WebSocket으로 전환하기 전에 읽기·쓰기 기한을 해제한다.
	// 연결 후에는 각 메시지를 보낼 때마다 쓰기 기한을 따로 둔다.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(900000)
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	defer conn.CloseNow()
	session := interpreter.New(ctx, s.client, *h.Model, interpreter.Options{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		session.Close()
		return
	}
	s.live = session
	s.conn = conn
	s.upgrading = false
	s.mu.Unlock()
	defer func() {
		session.Close()
		s.mu.Lock()
		if s.live == session {
			s.live = nil
			s.conn = nil
		}
		s.mu.Unlock()
	}()
	write := func(value any) error {
		data, e := json.Marshal(value)
		if e != nil {
			return e
		}
		sendCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		return conn.Write(sendCtx, websocket.MessageText, data)
	}
	if err = write(map[string]any{"type": "ready", "model": *h.Model, "languages": map[string]string{"user": "ko", "shop": "ja"}, "recognition": "incremental-whisper-windows", "max_window_seconds": 20, "engine": "go"}); err != nil {
		return
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-session.Done():
				conn.CloseNow()
				return
			case event := <-session.Events():
				if write(event) != nil {
					session.Close()
					conn.CloseNow()
					return
				}
			}
		}
	}()
	defer func() { session.Close(); conn.CloseNow(); <-writerDone }()
	for {
		kind, data, e := conn.Read(ctx)
		if e != nil {
			return
		}
		var message interpreter.Audio
		if kind != websocket.MessageText || json.Unmarshal(data, &message) != nil {
			err = errors.New("Invalid live audio JSON")
		} else {
			switch message.Type {
			case "user.stop_output":
				err = session.StopOutput(message.CommandID)
			case "playback.cleared":
				session.Cleared(message.CommandID)
				err = nil
			case "audio":
				if len(message.WAV) > 854000 {
					err = errors.New("Oversized audio")
					break
				}
				var wav []byte
				wav, err = base64.StdEncoding.Strict().DecodeString(message.WAV)
				if err == nil {
					err = ValidateWAV(wav)
				}
				if err == nil && message.Samples != (len(wav)-44)/2 {
					err = errors.New("Audio sample count differs")
				}
				if err == nil {
					err = session.Audio(message, wav)
				}
			default:
				err = errors.New("Invalid live request")
			}
		}
		if err != nil {
			_ = write(map[string]string{"type": "error", "message": err.Error()})
			_ = conn.Close(websocket.StatusPolicyViolation, "Invalid live request")
			return
		}
	}
}
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	live, conn := s.live, s.conn
	s.mu.Unlock()
	if live != nil {
		live.Close()
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
}
func synthesize(ctx context.Context, text string) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("Japanese synthesis requires macOS Kyoko")
	}
	dir, err := os.MkdirTemp("", "yoyaku-go-voice-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path := filepath.Join(dir, "speech.wav")
	cmd := exec.CommandContext(ctx, "/usr/bin/say", "-v", "Kyoko", "-o", path, "--file-format=WAVE", "--data-format=LEI16@22050")
	cmd.Stdin = strings.NewReader(text)
	if err = cmd.Run(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 16*1024*1024 {
		return nil, fmt.Errorf("Synthesized audio is too large")
	}
	return os.ReadFile(path)
}
