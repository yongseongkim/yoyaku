//go:build darwin && cgo

package nativewhisper

/*
#cgo CFLAGS: -I${SRCDIR}/include
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"yoyaku/internal/interpreter"
)

type Recognizer struct {
	mu  sync.Mutex
	api *C.yoyaku_whisper
}

func New(root, path string) (*Recognizer, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	library := C.CString(filepath.Join(root, ".local-native/whisper/whisper.framework/whisper"))
	defer C.free(unsafe.Pointer(library))
	model := C.CString(path)
	defer C.free(unsafe.Pointer(model))
	api := C.yoyaku_open(library, model)
	if api == nil {
		return nil, fmt.Errorf("native Whisper: %s", C.GoString(C.yoyaku_error()))
	}
	return &Recognizer{api: api}, nil
}
func (r *Recognizer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.api != nil {
		C.yoyaku_close(r.api)
		r.api = nil
	}
}
func (r *Recognizer) Transcribe(ctx context.Context, audio []float32, language, prompt string) (interpreter.Recognition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result interpreter.Recognition
	if r.api == nil {
		return result, errors.New("Whisper is closed")
	}
	if len(audio) < 4800 || len(audio) > 320000 || language != "ko" && language != "ja" {
		return result, errors.New("Invalid speech input")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	lang, hint := C.CString(language), C.CString(prompt)
	defer C.free(unsafe.Pointer(lang))
	defer C.free(unsafe.Pointer(hint))
	flag := C.yoyaku_cancel_new()
	if flag == nil {
		return result, errors.New("Cannot allocate native cancellation flag")
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { C.yoyaku_cancel_set(flag); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		C.yoyaku_cancel_free(flag)
	}()
	started := time.Now()
	code := C.yoyaku_transcribe(r.api, (*C.float)(unsafe.Pointer(&audio[0])), C.int(len(audio)), lang, hint, flag)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if code != 0 {
		return result, fmt.Errorf("Whisper inference failed (%d)", code)
	}
	var text strings.Builder
	words := []interpreter.Word{}
	for s := C.int(0); s < C.yoyaku_segments(r.api); s++ {
		segment := C.GoString(C.yoyaku_segment_text(r.api, s))
		text.WriteString(segment)
		segmentWords := []interpreter.Word{}
		pending := ""
		start := float64(C.yoyaku_segment_start(r.api, s)) / 100
		end := float64(C.yoyaku_segment_end(r.api, s)) / 100
		for t := C.int(0); t < C.yoyaku_tokens(r.api, s); t++ {
			token := C.yoyaku_token_data(r.api, s, t)
			if int(token.id) >= int(C.yoyaku_eot(r.api)) {
				continue
			}
			part := C.GoString(C.yoyaku_token_text(r.api, s, t))
			if pending == "" && token.t0 >= 0 {
				start = float64(token.t0) / 100
			}
			pending += part
			if !utf8.ValidString(pending) {
				continue
			}
			tokenEnd := float64(token.t1) / 100
			if tokenEnd <= 0 && len(segmentWords) > 0 {
				segmentWords[len(segmentWords)-1].Word += pending
				pending = ""
				continue
			}
			if strings.TrimSpace(pending) != "" && tokenEnd > 0 {
				segmentWords = append(segmentWords, interpreter.Word{Word: pending, Start: math.Max(0, start), End: math.Min(float64(len(audio))/16000, tokenEnd)})
			}
			pending = ""
		}
		if pending != "" && utf8.ValidString(pending) && len(segmentWords) > 0 {
			segmentWords[len(segmentWords)-1].Word += pending
		}
		if len(segmentWords) == 0 && strings.TrimSpace(segment) != "" && end > 0 {
			segmentWords = append(segmentWords, interpreter.Word{Word: segment, Start: float64(C.yoyaku_segment_start(r.api, s)) / 100, End: math.Min(end, float64(len(audio))/16000)})
		}
		words = append(words, segmentWords...)
	}
	result = interpreter.Recognition{Text: strings.TrimSpace(text.String()), Words: words, Language: language, LatencyMS: float64(time.Since(started).Microseconds()) / 1000, DurationMS: float64(len(audio)) / 16, PromptVersion: "reservation-asr-v2-native", Model: "whisper.cpp-small"}
	if result.Text == "" {
		return result, errors.New("인식한 말이 없습니다. 다시 말해 주세요.")
	}
	return result, nil
}
