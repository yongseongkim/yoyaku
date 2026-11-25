package interpreter

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var correction = regexp.MustCompile(`^(아니|아[,，]?\s*아니|정정|말고|いや|いえ|訂正|ではなく)`)

type candidate struct {
	text         string
	end, through int
}
type observation struct {
	meta     Audio
	text     string
	words    []Word
	previous string
}
type window struct {
	id                string
	number, sequence  int64
	samples, revision int
	text              string
	quiet             float64
	previous          string
	candidates        []candidate
	first             map[string]time.Time
	denied            map[string]bool
	agreed            int
	blocked           bool
	judge             *judgeJob
}

func candidates(text string, words []Word, samples int) []candidate {
	aligned := map[int]int{}
	cursor := 0
	for _, w := range words {
		token := strings.TrimSpace(w.Word)
		if token == "" || math.IsNaN(w.End) || math.IsInf(w.End, 0) || w.End <= 0 {
			continue
		}
		i := strings.Index(text[cursor:], token)
		if i < 0 {
			continue
		}
		cursor += i + len(token)
		through := int(math.Round(w.End * 16000))
		if through <= samples+160 {
			if through > samples {
				through = samples
			}
			aligned[cursor] = through
		}
	}
	ends := []int{}
	for i, r := range text {
		end := i + utf8.RuneLen(r)
		boundary := strings.ContainsRune("。！？!?", r)
		if r == '.' {
			next, _ := utf8.DecodeRuneInString(text[end:])
			boundary = end == len(text) || unicode.IsSpace(next)
		}
		if boundary {
			ends = append(ends, end)
		}
	}
	ends = append(ends, len(text))
	seen := map[int]bool{}
	result := []candidate{}
	for _, end := range ends {
		if seen[end] {
			continue
		}
		seen[end] = true
		if len(seen) > 8 {
			break
		}
		through, ok := aligned[end]
		if !ok || correction.MatchString(strings.TrimLeftFunc(text[end:], unicode.IsSpace)) {
			continue
		}
		result = append(result, candidate{strings.TrimSpace(text[:end]), end, through})
	}
	return result
}
func commonPrefix(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}
func (s *Session) eligible(w *window) []candidate {
	if w == nil || w.blocked {
		return nil
	}
	result := []candidate{}
	for _, c := range w.candidates {
		if c.end <= w.agreed && s.opts.Now().Sub(w.first[c.text]) >= s.opts.StableFor &&
			(w.quiet >= 700 || time.Duration(w.samples-c.through)*time.Second/16000 >= s.opts.Lookahead) && !w.denied[c.text] {
			result = append(result, c)
		}
	}
	return result
}
