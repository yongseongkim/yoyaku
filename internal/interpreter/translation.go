package interpreter

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

func stopCaptionTimers(c *caption) {
	if c.debounce != nil {
		c.debounce.Stop()
		c.debounce = nil
	}
	if c.maxWait != nil {
		c.maxWait.Stop()
		c.maxWait = nil
	}
}
func (s *Session) updateCaption(id, text string, revision int, committed bool) {
	c := s.captions[id]
	if c == nil {
		// 확정되고 번역까지 끝난 자막부터 비운다. 비울 자리가 없으면 새 자막 처리를 중단한다.
		for len(s.captionOrder) >= 64 {
			removed := false
			for i, oldID := range s.captionOrder {
				old := s.captions[oldID]
				if old.committed && old.job == nil {
					stopCaptionTimers(old)
					delete(s.captions, oldID)
					s.captionOrder = append(s.captionOrder[:i], s.captionOrder[i+1:]...)
					removed = true
					break
				}
			}
			if !removed {
				s.recovery("shop", "caption_capacity", "자막 처리가 밀리고 있습니다. 통역을 다시 시작해 주세요.")
				return
			}
		}
		c = &caption{id: id, order: s.captionCount}
		s.captionCount++
		s.captions[id] = c
		s.captionOrder = append(s.captionOrder, id)
	}
	changed := revision > c.revision
	if changed && c.lastRevision > 0 && !strings.HasPrefix(text, c.text) {
		s.emit(Event{"type": "caption.invalidate", "track": "shop", "segment_id": id, "previous_source_revision": c.lastRevision, "current_revision": revision, "reason": "source_replaced"})
	}
	c.text = text
	c.revision = revision
	c.committed = committed
	if c.job != nil && !strings.HasPrefix(text, c.job.text) {
		c.job.cancel()
		c.job = nil
	}
	if committed {
		stopCaptionTimers(c)
		if c.lastRevision == revision && c.lastText != "" {
			s.caption(c, revision, c.lastText, false)
		} else {
			s.startCaption(c)
		}
		return
	}
	if !changed {
		return
	}
	if c.debounce != nil {
		c.debounce.Stop()
	}
	c.debounce = time.AfterFunc(s.opts.CaptionDebounce, func() { s.mu.Lock(); defer s.mu.Unlock(); s.startCaption(c) })
	if c.maxWait == nil {
		c.maxWait = time.AfterFunc(s.opts.CaptionMaxWait, func() { s.mu.Lock(); defer s.mu.Unlock(); s.startCaption(c) })
	}
}
func (s *Session) caption(c *caption, revision int, text string, provisional bool) {
	s.emit(Event{"type": "caption.update", "track": "shop", "segment_id": c.id, "source_revision": revision, "current_revision": c.revision, "text": text, "provisional": provisional, "source_sequence": c.order})
}
func (s *Session) startCaption(c *caption) {
	stopCaptionTimers(c)
	if s.closed || s.captions[c.id] != c || c.job != nil || c.lastRevision == c.revision {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Deadline)
	job := &translationJob{text: c.text, revision: c.revision, cancel: cancel}
	c.job = job
	go func() {
		result, err := s.backend.Translate(ctx, TranslationInput{job.text, "ja-JP", "ko-KR"}, s.model)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer cancel()
		if s.closed || c.job != job {
			return
		}
		c.job = nil
		if err != nil || ctx.Err() != nil || !validText(result.Text) {
			s.recovery("shop", "translation_error", "한국어 자막 번역에 실패했습니다. 다음 원문 갱신 때 다시 처리합니다.")
			return
		}
		if !strings.HasPrefix(c.text, job.text) {
			return
		}
		c.lastText = result.Text
		c.lastRevision = job.revision
		s.caption(c, job.revision, result.Text, !c.committed || c.revision != job.revision)
		if c.revision > job.revision {
			s.startCaption(c)
		}
	}()
}
func validText(text string) bool {
	return strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= 8192
}
func (s *Session) drainUser() {
	if s.closed || s.userJob != nil || s.userBlocked || len(s.userQueue) == 0 {
		return
	}
	item := s.userQueue[0]
	s.userQueue = s.userQueue[1:]
	ctx, cancel := context.WithTimeout(s.ctx, s.opts.Deadline)
	job := &translationJob{text: item.text, generation: item.generation, revision: 1, cancel: cancel}
	s.userJob = job
	go func() {
		result, err := s.backend.Translate(ctx, TranslationInput{job.text, "ko-KR", "ja-JP"}, s.model)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer cancel()
		if s.closed || s.userJob != job || s.generation != job.generation {
			return
		}
		s.userJob = nil
		if err != nil || ctx.Err() != nil || !validText(result.Text) {
			s.userBlocked = true
			s.recovery("user", "translation_error", "번역에 실패해 일본어 출력이 멈췄습니다. 원문을 확인하고 일본어 음성 중단을 눌러 다시 말해 주세요.")
			return
		}
		e := Event{"type": "tts.request", "track": "user", "segment_id": item.id, "source_revision": 1, "source_text": item.text, "text": result.Text, "language": "ja-JP", "generation": job.generation, "sequence": item.sequence}
		if s.clearID != "" {
			if len(s.held) >= 8 {
				s.emit(Event{"type": "error", "message": "음성 중단 확인이 없어 통역을 종료합니다."})
				s.closeLocked()
				return
			}
			s.held = append(s.held, e)
		} else {
			s.emit(e)
		}
		s.drainUser()
	}()
}
