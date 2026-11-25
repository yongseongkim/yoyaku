package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http/httptest"
	"testing"
	"yoyaku/internal/interpreter"
)

type fakeRecognizer struct {
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (f *fakeRecognizer) Transcribe(ctx context.Context, audio []float32, language, prompt string) (interpreter.Recognition, error) {
	f.calls++
	if f.entered != nil {
		close(f.entered)
		select {
		case <-ctx.Done():
			return interpreter.Recognition{}, ctx.Err()
		case <-f.release:
		}
	}
	return interpreter.Recognition{Text: "두 명 예약하고 싶습니다.", Words: []interpreter.Word{{Word: "두 명", Start: 0, End: .4}}}, nil
}
func wav(quiet bool) []byte {
	b := make([]byte, 32044)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 32000)
	if !quiet {
		for i := 44; i < len(b); i += 2 {
			binary.LittleEndian.PutUint16(b[i:], 1000)
		}
	}
	return b
}
func TestSpeechProtocolAndValidation(t *testing.T) {
	f := &fakeRecognizer{}
	s := New(f)
	for _, c := range []struct {
		name, lang, origin string
		data               []byte
		want               int
	}{{"valid", "ko", "", wav(false), 200}, {"Japanese", "ja", "", wav(false), 200}, {"language required", "", "", wav(false), 400}, {"invalid wav", "ko", "", []byte("bad"), 400}, {"silent", "ko", "", wav(true), 422}, {"browser blocked", "ko", "http://bad", wav(false), 403}} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/transcribe", bytes.NewReader(c.data))
			r.Header.Set("X-Source-Language", c.lang)
			r.Header.Set("Origin", c.origin)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
		})
	}
	if f.calls != 2 {
		t.Fatal("Invalid audio reached recognition")
	}
}
func TestSpeechDoesNotQueueUnboundedAudio(t *testing.T) {
	f := &fakeRecognizer{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(f)
	done := make(chan struct{})
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/transcribe", bytes.NewReader(wav(false)))
		r.Header.Set("X-Source-Language", "ko")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	go func() { defer close(done); request() }()
	<-f.entered
	if w := request(); w.Code != 429 {
		t.Fatal(w.Code)
	}
	close(f.release)
	<-done
}
func TestShortPartialWindowsDoNotPrimeVocabularyEcho(t *testing.T) {
	if hint("ja", 16000) != "" || hint("ko", 31999) != "" {
		t.Fatal("Short window received vocabulary prompt")
	}
	if hint("ja", 32000) == "" || hint("ko", 64000) == "" {
		t.Fatal("Long window lost domain vocabulary")
	}
}
