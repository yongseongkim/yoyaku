// scenario 패키지는 예약 연습과 자동 테스트가 함께 쓰는 대화를 읽어 온다.
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Turn struct {
	ID       string `json:"id"`
	Speaker  string `json:"speaker"`
	Text     string `json:"text"`
	KO       string `json:"ko"`
	Japanese string `json:"spoken_ja"`
}
type Scenario struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Checks      []string `json:"checks"`
	Turns       []Turn   `json:"turns"`
}
type Catalog struct {
	Version   int        `json:"schema_version"`
	Contract  string     `json:"contract"`
	Scenarios []Scenario `json:"scenarios"`
}

func Load(root string) (Catalog, error) {
	var c Catalog
	data, err := os.ReadFile(filepath.Join(root, "testdata/reservations/scenarios.json"))
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Catalog) Validate() error {
	if c.Version != 1 || len(c.Scenarios) == 0 {
		return errors.New("Invalid reservation catalog")
	}
	ids := map[string]bool{}
	for _, s := range c.Scenarios {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(s.ID) || ids[s.ID] || strings.TrimSpace(s.Title) == "" || s.Category == "" || s.Description == "" || len(s.Checks) == 0 || len(s.Turns) == 0 {
			return fmt.Errorf("Invalid or duplicate reservation case: %s", s.ID)
		}
		ids[s.ID] = true
		turns := map[string]bool{}
		speakers := map[string]bool{}
		for _, check := range s.Checks {
			if strings.TrimSpace(check) == "" {
				return fmt.Errorf("Empty manual check: %s", s.ID)
			}
		}
		for _, t := range s.Turns {
			if t.ID == "" || turns[t.ID] || t.KO == "" || t.Speaker != "A" && t.Speaker != "B" || t.Speaker == "A" && t.Text == "" || t.Speaker == "B" && t.Japanese == "" {
				return fmt.Errorf("Invalid turn: %s/%s", s.ID, t.ID)
			}
			turns[t.ID] = true
			speakers[t.Speaker] = true
		}
		if !speakers["A"] || !speakers["B"] {
			return fmt.Errorf("Case needs both A and B: %s", s.ID)
		}
	}
	return nil
}
func (c Catalog) Find(id string) (Scenario, bool) {
	for _, s := range c.Scenarios {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}
