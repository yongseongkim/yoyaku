// interpreter 패키지는 인식 원문의 수정과 확정, 음성 재생과 취소를 관리한다.
// 콜백에서 세션 상태를 바꿀 때는 잠금을 잡는다. 모델 호출과 소켓 통신은 잠금을 풀고 실행하며,
// 대기열의 크기를 제한하고 세션이 끝나면 작업을 취소한다.
package interpreter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type judgeJob struct {
	text   string
	cancel context.CancelFunc
}
type translationJob struct {
	text                 string
	revision, generation int
	cancel               context.CancelFunc
}
type caption struct {
	id, text, lastText            string
	revision, lastRevision, order int
	committed                     bool
	job                           *translationJob
	debounce, maxWait             *time.Timer
}
type outgoing struct {
	id, text             string
	generation, sequence int
}
type Session struct {
	mu                         sync.Mutex
	ctx                        context.Context
	cancel                     context.CancelFunc
	backend                    Backend
	model                      string
	opts                       Options
	started                    time.Time
	events                     chan Event
	closed                     bool
	eventSequence              int64
	windows                    map[string]*window
	working                    map[string]bool
	retired                    map[string]int64
	lastShop                   string
	captions                   map[string]*caption
	captionOrder               []string
	captionCount               int
	userQueue                  []outgoing
	userJob                    *translationJob
	userBlocked                bool
	generation, outputSequence int
	clearID                    string
	held                       []Event
	stops                      map[string]bool
}

func New(ctx context.Context, backend Backend, model string, opts Options) *Session {
	opts = opts.defaults()
	ctx, cancel := context.WithCancel(ctx)
	return &Session{ctx: ctx, cancel: cancel, backend: backend, model: model, opts: opts, started: opts.Now(), events: make(chan Event, 128), windows: map[string]*window{}, working: map[string]bool{}, retired: map[string]int64{}, captions: map[string]*caption{}, stops: map[string]bool{}}
}
func (s *Session) Events() <-chan Event  { return s.events }
func (s *Session) Done() <-chan struct{} { return s.ctx.Done() }
func (s *Session) emit(e Event) {
	if s.closed || s.ctx.Err() != nil {
		return
	}
	s.eventSequence++
	e["event_sequence"] = s.eventSequence
	e["at_ms"] = s.opts.Now().Sub(s.started).Milliseconds()
	e["model"] = s.model
	select {
	case s.events <- e:
	default:
		s.closeLocked()
	}
}
func (s *Session) recovery(track, reason, message string) {
	s.emit(Event{"type": "recovery", "track": track, "reason": reason, "message": message})
}
func windowNumber(meta Audio) (int64, error) {
	if meta.Track != "user" && meta.Track != "shop" {
		return 0, errors.New("Invalid audio track")
	}
	prefix := meta.Track + "-"
	if !strings.HasPrefix(meta.WindowID, prefix) {
		return 0, errors.New("Invalid audio window")
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(meta.WindowID, prefix), 10, 64)
	if err != nil || n < 1 || n > 9007199254740991 || meta.WindowID != fmt.Sprintf("%s%d", prefix, n) || meta.Sequence < 1 || meta.Sequence > 9007199254740991 || meta.Samples < 4800 || meta.Samples > 320000 || math.IsNaN(meta.QuietMS) || math.IsInf(meta.QuietMS, 0) || meta.QuietMS < 0 || meta.QuietMS > 5000 {
		return 0, errors.New("Invalid audio window metadata")
	}
	return n, nil
}
func (s *Session) Audio(meta Audio, wav []byte) error {
	n, err := windowNumber(meta)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return nil
	}
	processed := func(reason string) {
		s.emit(Event{"type": "audio.processed", "track": meta.Track, "window_id": meta.WindowID, "sequence": meta.Sequence, "reason": reason})
	}
	if n <= s.retired[meta.Track] {
		processed("obsolete")
		return nil
	}
	if s.working[meta.Track] {
		processed("busy")
		return nil
	}
	w := s.windows[meta.Track]
	if w != nil && w.id != meta.WindowID {
		return errors.New("Unconsumed audio must be continued or explicitly discarded")
	}
	if w == nil {
		w = &window{id: meta.WindowID, number: n, first: map[string]time.Time{}, denied: map[string]bool{}}
		s.windows[meta.Track] = w
	}
	if meta.Sequence <= w.sequence {
		processed("duplicate")
		return nil
	}
	if meta.Samples < w.samples {
		return errors.New("Audio cannot shrink without consumption")
	}
	w.sequence = meta.Sequence
	w.samples = meta.Samples
	s.working[meta.Track] = true
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Deadline)
	go func() {
		defer cancel()
		language := "ko"
		if meta.Track == "shop" {
			language = "ja"
		}
		r, err := s.backend.Transcribe(ctx, wav, language, true)
		if err == nil {
			err = ctx.Err()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.working[meta.Track] = false
		if s.closed {
			return
		}
		if s.windows[meta.Track] == w {
			text := strings.TrimSpace(r.Text)
			if err != nil {
				s.recovery(meta.Track, "recognition_error", err.Error())
			} else if text == "" || utf8.RuneCountInString(text) > 4000 || r.Words == nil {
				s.recovery(meta.Track, "recognition_error", "Missing aligned recognition text")
			} else {
				if text != w.text {
					w.revision++
				}
				s.emit(Event{"type": "transcript", "track": meta.Track, "window_id": w.id, "revision": w.revision, "sequence": meta.Sequence, "text": text, "latency_ms": r.LatencyMS, "stt_prompt_version": r.PromptVersion})
				if meta.Track == "shop" {
					s.lastShop = text
					s.updateCaption(w.id, text, w.revision, false)
				}
				s.observe(meta.Track, w, observation{meta: meta, text: text, words: r.Words, previous: s.lastShop})
			}
		}
		processed("processed")
	}()
	return nil
}

func (s *Session) observe(track string, w *window, o observation) {
	first := map[string]time.Time{}
	list := candidates(o.text, o.words, o.meta.Samples)
	for _, c := range list {
		t, ok := w.first[c.text]
		if !ok {
			t = s.opts.Now()
		}
		first[c.text] = t
	}
	denied := map[string]bool{}
	for text := range w.denied {
		if _, ok := first[text]; ok {
			denied[text] = true
		}
	}
	w.agreed = commonPrefix(w.text, o.text)
	w.text = o.text
	w.first = first
	w.denied = denied
	w.candidates = list
	w.samples = o.meta.Samples
	w.quiet = o.meta.QuietMS
	w.previous = o.previous
	w.blocked = false
	if w.judge != nil {
		valid := false
		for _, c := range s.eligible(w) {
			if c.text == w.judge.text {
				valid = true
			}
		}
		if !valid {
			w.judge.cancel()
		}
	}
	s.decide(track, w)
}
func (s *Session) decide(track string, w *window) {
	if s.closed || w.judge != nil || s.windows[track] != w {
		return
	}
	eligible := s.eligible(w)
	if len(eligible) == 0 {
		reason := "asr_not_stable"
		if len(w.denied) > 0 {
			reason = "meaning_incomplete"
		}
		s.emit(Event{"type": "chunk.wait", "track": track, "window_id": w.id, "reason": reason, "text": w.text})
		return
	}
	c := eligible[0]
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Deadline)
	job := &judgeJob{text: c.text, cancel: cancel}
	w.judge = job
	input := ChunkInput{Text: c.text, Language: "ja-JP"}
	if track == "user" {
		input.Language = "ko-KR"
		input.Previous = w.previous
	}
	s.emit(Event{"type": "chunk.request", "track": track, "window_id": w.id, "text": job.text})
	go func() {
		result, err := s.backend.Judge(ctx, input, s.model)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer cancel()
		valid := !s.closed && s.windows[track] == w && ctx.Err() == nil
		if !s.closed && s.windows[track] == w && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			w.blocked = true
			s.recovery(track, "chunk_model_error", "의미 판단 시간이 초과됐습니다. 원문을 확인하고 다시 말해 주세요.")
		}
		if valid {
			label := strings.TrimSpace(result.Text)
			if err != nil || (label != "R" && label != "W") {
				w.blocked = true
				message := "Invalid chunk label"
				if err != nil {
					message = err.Error()
				}
				s.recovery(track, "chunk_model_error", message)
			} else {
				s.emit(Event{"type": "chunk.decision", "track": track, "window_id": w.id, "text": job.text, "label": label, "prompt_version": result.PromptVersion, "latency_ms": result.LatencyMS})
				for _, current := range s.eligible(w) {
					if current.text != job.text {
						continue
					}
					if label == "W" {
						w.denied[job.text] = true
					} else {
						s.commit(track, w, current)
					}
					break
				}
			}
		}
		if w.judge == job {
			w.judge = nil
		}
		if s.windows[track] == w {
			s.decide(track, w)
		}
	}()
}
func (s *Session) commit(track string, w *window, c candidate) {
	through := c.through
	if c.end == len(w.text) && w.quiet >= 700 {
		through = w.samples
	}
	revision := 1
	if track == "shop" {
		revision = w.revision
		if w.text != c.text {
			revision++
		}
		s.updateCaption(w.id, c.text, revision, true)
	} else {
		if len(s.userQueue) >= 64 {
			s.recovery(track, "output_capacity", "출력 대기열이 가득 찼습니다. 일본어 음성을 중단하고 다시 말해 주세요.")
			w.blocked = true
			return
		}
		s.outputSequence++
		s.userQueue = append(s.userQueue, outgoing{w.id, c.text, s.generation, s.outputSequence})
		s.drainUser()
	}
	s.emit(Event{"type": "source.commit", "track": track, "window_id": w.id, "sequence": w.sequence, "revision": revision, "text": c.text, "through_samples": through})
	s.retired[track] = w.number
	s.windows[track] = nil
	s.emit(Event{"type": "audio.consume", "track": track, "window_id": w.id, "through_samples": through})
}
func (s *Session) StopOutput(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || id == "" || len(id) > 80 || s.stops[id] {
		return nil
	}
	if len(s.stops) >= 256 {
		return ErrCapacity
	}
	s.stops[id] = true
	s.held = nil
	s.userQueue = nil
	s.userBlocked = false
	if s.userJob != nil {
		s.userJob.cancel()
		s.userJob = nil
	}
	if w := s.windows["user"]; w != nil {
		if w.judge != nil {
			w.judge.cancel()
		}
		s.retired["user"] = w.number
		s.windows["user"] = nil
		s.emit(Event{"type": "audio.discard", "track": "user", "window_id": w.id})
	}
	previous := s.generation
	s.generation++
	s.clearID = id
	s.emit(Event{"type": "playback.clear", "command_id": id, "cancelled_generation": previous, "next_generation": s.generation, "reason": "user_requested"})
	return nil
}
func (s *Session) Cleared(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || id != s.clearID {
		return
	}
	s.clearID = ""
	for _, e := range s.held {
		if e["generation"] == s.generation {
			s.emit(e)
		}
	}
	s.held = nil
}
func (s *Session) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.closeLocked() }
func (s *Session) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	s.userQueue = nil
	s.held = nil
	for _, c := range s.captions {
		stopCaptionTimers(c)
	}
}
