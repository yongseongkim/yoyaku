//go:build !darwin || !cgo

package nativewhisper

import (
	"context"
	"errors"
	"yoyaku/internal/interpreter"
)

type Recognizer struct{}

func New(string, string) (*Recognizer, error) {
	return nil, errors.New("Native speech currently requires macOS and CGO")
}
func (*Recognizer) Close() {}
func (*Recognizer) Transcribe(context.Context, []float32, string, string) (interpreter.Recognition, error) {
	return interpreter.Recognition{}, errors.New("Native speech unavailable")
}
