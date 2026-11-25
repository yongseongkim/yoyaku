package speech

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"
	"yoyaku/internal/app"
	"yoyaku/internal/interpreter"
)

type Recognizer interface {
	Transcribe(context.Context, []float32, string, string) (interpreter.Recognition, error)
}
type Server struct {
	recognizer Recognizer
	busy       chan struct{}
}

func New(r Recognizer) *Server { return &Server{r, make(chan struct{}, 1)} }

var prompts = map[string]string{"ko": "음식점 예약 전화입니다. 예약, 날짜, 시간, 인원, 성함, 변경, 취소 수수료, 알레르기.", "ja": "飲食店への予約の電話です。予約、何名様、日時、人数、お名前、満席、変更、キャンセル料、アレルギー。"}

// 짧은 녹음에 예약 어휘를 힌트로 주면 실제로 하지 않은 말까지 그 어휘로 인식할 수 있다.
func hint(language string, samples int) string {
	if samples < 32000 {
		return ""
	}
	return prompts[language]
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, text string) {
	reply(w, status, map[string]string{"error": text})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/health" {
		reply(w, 200, map[string]any{"ready": true, "local": true, "engine": "go+whisper.cpp", "languages": []string{"ko", "ja"}, "incremental_windows": true, "native_streaming": false, "prompt_version": "reservation-asr-v2-native", "model": "whisper.cpp-small"})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/transcribe" {
		fail(w, 404, "Not found")
		return
	}
	if r.Header.Get("Origin") != "" {
		fail(w, 403, "Use the app proxy")
		return
	}
	language := r.Header.Get("X-Source-Language")
	if _, ok := prompts[language]; !ok {
		fail(w, 400, "Use explicit ko or ja language")
		return
	}
	select {
	case s.busy <- struct{}{}:
		defer func() { <-s.busy }()
	default:
		fail(w, 429, "Speech recognition is busy")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 640044))
	if err != nil {
		fail(w, 413, "Audio too large")
		return
	}
	if err = app.ValidateWAV(data); err != nil {
		fail(w, 400, err.Error())
		return
	}
	audio := make([]float32, (len(data)-44)/2)
	var energy float64
	for i := range audio {
		v := float32(int16(binary.LittleEndian.Uint16(data[44+i*2:]))) / 32768
		audio[i] = v
		energy += float64(v * v)
	}
	if math.Sqrt(energy/float64(len(audio))) < 0.002 {
		fail(w, 422, "목소리가 들리지 않았습니다. 다시 말해 주세요.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.recognizer.Transcribe(ctx, audio, language, hint(language, len(audio)))
	if err != nil {
		fail(w, 422, err.Error())
		return
	}
	if language == "ko" {
		result.Language = "ko-KR"
	} else {
		result.Language = "ja-JP"
	}
	if r.Header.Get("X-Word-Timestamps") != "1" {
		result.Words = nil
	}
	reply(w, 200, result)
}
