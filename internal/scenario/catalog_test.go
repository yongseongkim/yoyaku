package scenario

import (
	"os"
	"strings"
	"testing"
)

func TestReservationCatalog(t *testing.T) {
	c, e := Load("../..")
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Scenarios) != 15 {
		t.Fatal("Update the case inventory when intentionally adding/removing scripts")
	}
	for _, id := range []string{"normal-booking", "alternative-declined", "wrong-date-readback", "party-mismatch", "stt-revision-pause", "fee-and-cancellation-terms", "seat-and-allergy", "cancel-existing-booking", "hold-and-transfer"} {
		s, ok := c.Find(id)
		if !ok || len(s.Checks) < 3 {
			t.Fatal("Missing coverage", id)
		}
	}
	data, e := os.ReadFile("../../testdata/reservations/scenarios.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, retired := range []string{"policy_decision", "suggest_reply", "requires_approval", "policy_context"} {
		if strings.Contains(string(data), retired) {
			t.Fatal("Autonomous-agent labels must not return", retired)
		}
	}
}
func TestInvalidReservationCasesAreRejected(t *testing.T) {
	for _, kind := range []string{"duplicate", "speaker", "missing-reference", "empty-check"} {
		t.Run(kind, func(t *testing.T) {
			c, e := Load("../..")
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "duplicate":
				c.Scenarios = append(c.Scenarios, c.Scenarios[0])
			case "speaker":
				c.Scenarios[0].Turns[0].Speaker = "C"
			case "missing-reference":
				c.Scenarios[0].Turns[0].KO = ""
			case "empty-check":
				c.Scenarios[0].Checks[0] = " "
			}
			if c.Validate() == nil {
				t.Fatal("Invalid case accepted")
			}
		})
	}
}
