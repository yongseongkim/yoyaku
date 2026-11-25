package interpreter

import (
	"context"
	"errors"
	"time"
	"yoyaku/internal/decision"
)

type Event map[string]any

type Audio struct {
	Type      string  `json:"type"`
	Track     string  `json:"track"`
	WindowID  string  `json:"window_id"`
	Sequence  int64   `json:"sequence"`
	Samples   int     `json:"samples"`
	QuietMS   float64 `json:"quiet_ms"`
	WAV       string  `json:"wav"`
	CommandID string  `json:"command_id"`
}

type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}
type Recognition struct {
	Text          string  `json:"text"`
	Words         []Word  `json:"words"`
	Language      string  `json:"language"`
	LatencyMS     float64 `json:"latency_ms"`
	DurationMS    float64 `json:"audio_duration_ms"`
	PromptVersion string  `json:"prompt_version"`
	Model         string  `json:"model"`
}
type ChunkInput = decision.Input
type TranslationInput struct {
	Text   string `json:"text"`
	Source string `json:"source_language"`
	Target string `json:"target_language"`
}
type Result struct {
	Text          string
	LatencyMS     float64
	PromptVersion string
}
type Backend interface {
	Transcribe(context.Context, []byte, string, bool) (Recognition, error)
	Judge(context.Context, ChunkInput, string) (Result, error)
	Translate(context.Context, TranslationInput, string) (Result, error)
}
type Options struct {
	Now             func() time.Time
	StableFor       time.Duration
	Lookahead       time.Duration
	CaptionDebounce time.Duration
	CaptionMaxWait  time.Duration
	Deadline        time.Duration
}

func (o Options) defaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StableFor <= 0 {
		o.StableFor = 600 * time.Millisecond
	}
	if o.Lookahead <= 0 {
		o.Lookahead = 400 * time.Millisecond
	}
	if o.CaptionDebounce <= 0 {
		o.CaptionDebounce = 120 * time.Millisecond
	}
	if o.CaptionMaxWait <= 0 {
		o.CaptionMaxWait = 350 * time.Millisecond
	}
	if o.Deadline <= 0 {
		o.Deadline = 15 * time.Second
	}
	return o
}

var ErrCapacity = errors.New("live session capacity reached")
