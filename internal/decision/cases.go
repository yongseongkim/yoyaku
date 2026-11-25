// decision 패키지는 지금까지 들은 말을 번역해도 될지 판단할 때 쓰는 입력과 테스트 사례를 정의한다.
// 모델은 번역 시점만 판단하고, 언제 끼어들지, 어떻게 답할지, 예약할지는 사용자가 정한다.
package decision

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Input struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	Previous string `json:"previous_utterance,omitempty"`
}

func ParseLabel(text string) (string, error) {
	if text != "R" && text != "W" {
		return "", errors.New("Decision model must return exactly R or W")
	}
	return text, nil
}

type Snapshot struct {
	AtMS      int64   `json:"at_ms"`
	Text      string  `json:"text"`
	QuietMS   float64 `json:"quiet_ms"`
	Samples   int     `json:"samples"`
	WordEndMS int     `json:"word_end_ms"`
	Expected  string  `json:"expected"` // “-”는 판단 요청이 없는 시점, R과 W는 한 번 요청해 해당 응답을 받는 시점이다.
	Commit    string  `json:"commit,omitempty"`
}
type Case struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Track     string     `json:"track"`
	Language  string     `json:"language"`
	Snapshots []Snapshot `json:"snapshots"`
}
type Catalog struct {
	Version int    `json:"schema_version"`
	Cases   []Case `json:"cases"`
}

func Load(root string) (Catalog, error) {
	var c Catalog
	b, e := os.ReadFile(filepath.Join(root, "testdata/decision/streams.json"))
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func (c Catalog) Validate() error {
	if c.Version != 1 || len(c.Cases) == 0 {
		return errors.New("Invalid decision stream catalog")
	}
	ids := map[string]bool{}
	for _, s := range c.Cases {
		if s.ID == "" || ids[s.ID] || s.Title == "" || len(s.Snapshots) < 2 || s.Track != "shop" && s.Track != "user" || s.Track == "shop" && s.Language != "ja-JP" || s.Track == "user" && s.Language != "ko-KR" {
			return fmt.Errorf("Invalid stream case: %s", s.ID)
		}
		ids[s.ID] = true
		previous := int64(-1)
		samples := 0
		committed := false
		for _, v := range s.Snapshots {
			if committed || v.AtMS < previous || v.AtMS < 0 || v.Text == "" || v.Samples < 4800 || v.Samples > 320000 || v.Samples < samples || v.WordEndMS < 1 || v.WordEndMS*16 > v.Samples || v.QuietMS < 0 || v.QuietMS > 5000 || v.Expected != "-" && v.Expected != "R" && v.Expected != "W" || v.Commit != "" && v.Expected != "R" {
				return fmt.Errorf("Invalid snapshot: %s @%d", s.ID, v.AtMS)
			}
			previous = v.AtMS
			samples = v.Samples
			committed = v.Commit != ""
		}
	}
	return nil
}
func (c Catalog) Find(id string) (Case, bool) {
	for _, v := range c.Cases {
		if v.ID == id {
			return v, true
		}
	}
	return Case{}, false
}
