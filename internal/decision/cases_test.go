package decision

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecisionStreamCatalog(t *testing.T) {
	c, e := Load("../..")
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Cases) != 12 {
		t.Fatal("Update the inventory when intentionally changing coverage")
	}
	for _, id := range []string{"ja-greeting", "ja-no-reply-needed", "ja-incomplete-tail", "ko-condition-pause", "ko-asr-revision", "ja-wait-for-quiet"} {
		if _, ok := c.Find(id); !ok {
			t.Fatal(id)
		}
	}
}
func TestModelInputContainsOnlyObservedSource(t *testing.T) {
	b, e := json.Marshal(Input{Text: "何名様でしょうか。", Language: "ja-JP"})
	if e != nil {
		t.Fatal(e)
	}
	for _, hidden := range []string{"expected", "snapshots", "commit", "reference", "checks"} {
		if strings.Contains(string(b), hidden) {
			t.Fatal("Reference leaked into model input")
		}
	}
}
func TestStrictDecisionLabels(t *testing.T) {
	for _, s := range []string{"R", "W"} {
		if _, e := ParseLabel(s); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []string{"respond", "cut", "READY", "RW", "R explanation", "", " R"} {
		if _, e := ParseLabel(s); e == nil {
			t.Fatal("Invalid label accepted", s)
		}
	}
}
func TestInvalidStreamCasesRejected(t *testing.T) {
	for _, mutation := range []string{"duplicate", "clock", "post-commit", "language", "shrinking", "label"} {
		t.Run(mutation, func(t *testing.T) {
			c, e := Load("../..")
			if e != nil {
				t.Fatal(e)
			}
			switch mutation {
			case "duplicate":
				c.Cases = append(c.Cases, c.Cases[0])
			case "clock":
				c.Cases[0].Snapshots[1].AtMS = -1
			case "post-commit":
				c.Cases[0].Snapshots = append(c.Cases[0].Snapshots, c.Cases[0].Snapshots[0])
			case "language":
				c.Cases[0].Language = "ko-KR"
			case "shrinking":
				c.Cases[0].Snapshots[1].Samples = 16000
			case "label":
				c.Cases[0].Snapshots[1].Expected = "cut"
			}
			if c.Validate() == nil {
				t.Fatal("Invalid stream accepted")
			}
		})
	}
}
