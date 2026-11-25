package interpreter

import (
	"testing"
	"yoyaku/internal/scenario"
)

func TestAllReservationDialoguesThroughGo(t *testing.T) {
	fixtures, err := scenario.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	var shops, users int
	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			h := setup(t, &fakeBackend{})
			numbers := map[string]int64{}
			for _, turn := range scenario.Turns {
				track, text := "shop", turn.Text
				if turn.Speaker == "B" {
					track, text = "user", turn.KO
				}
				numbers[track]++
				h.commit(track, text, numbers[track])
				kind := "caption.update"
				if track == "user" {
					kind = "tts.request"
					users++
				} else {
					shops++
				}
				e := h.next(kind, func(e Event) bool { return track == "user" || e["provisional"] == false })
				if e["text"] != "translated:"+text {
					t.Fatalf("source delivery changed: %#v", e)
				}
			}
		})
	}
	if shops != 43 || users != 44 {
		t.Fatalf("missing fixture turns: A=%d B=%d", shops, users)
	}
}
