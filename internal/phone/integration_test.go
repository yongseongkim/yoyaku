package phone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type harness struct {
	t                     *testing.T
	mock                  *Mock
	mockServer, appServer *httptest.Server
	app                   *Application
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := NewMock(ctx)
	m.Delay = 5 * time.Millisecond
	ms := httptest.NewServer(m)
	var app *Application
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	app = NewApplication(ctx, "../..", as.URL, ms.URL, m)
	h := &harness{t, m, ms, as, app}
	t.Cleanup(func() { app.Close(); m.Close(); cancel(); as.Close(); ms.Close() })
	return h
}
func (h *harness) request(path string, body any, key string) (int, map[string]any) {
	h.t.Helper()
	method := "GET"
	var reader io.Reader
	if body != nil {
		method = "POST"
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, h.appServer.URL+path, reader)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, e := http.DefaultClient.Do(r)
	if e != nil {
		h.t.Fatal(e)
	}
	defer response.Body.Close()
	var result map[string]any
	if e = json.NewDecoder(response.Body).Decode(&result); e != nil {
		h.t.Fatal(e)
	}
	return response.StatusCode, result
}
func (h *harness) create(scenario string) string {
	r := DefaultReservation()
	r.Scenario = scenario
	status, b := h.request("/api/calls", r, uuid())
	if status != 201 {
		h.t.Fatalf("create: %d %v", status, b)
	}
	return b["id"].(string)
}
func (h *harness) until(id string, predicate func(map[string]any) bool) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var s map[string]any
	for time.Now().Before(deadline) {
		_, s = h.request("/api/calls/"+id, nil, "")
		if predicate(s) {
			return s
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("Timed out: %v", s)
	return nil
}
func (h *harness) ready(id, stage string) map[string]any {
	return h.until(id, func(s map[string]any) bool {
		return s["stage"] == stage && s["status"] == "in-progress" && s["replyBusy"] == false
	})
}
func (h *harness) reply(id, action string) {
	h.t.Helper()
	_, s := h.request("/api/calls/"+id, nil, "")
	for _, raw := range s["suggestions"].([]any) {
		p := raw.(map[string]any)
		if p["action"] == action {
			status, b := h.request("/api/calls/"+id+"/replies", map[string]any{"text": p["ko"], "expected_reservation_revision": s["reservation"].(map[string]any)["revision"]}, "")
			if status != 202 {
				h.t.Fatalf("reply %s: %d %v", action, status, b)
			}
			return
		}
	}
	h.t.Fatalf("Missing %s at %v", action, s)
}
func TestReservationAcrossSignedWebhooksAndMedia(t *testing.T) {
	for _, scenario := range []string{"available", "alternative", "clarification"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t)
			id := h.create(scenario)
			h.ready(id, "greeting")
			h.reply(id, "request")
			if scenario == "alternative" {
				offer := h.ready(id, "offer")
				if offer["reservation"].(map[string]any)["status"] != "proposed" {
					t.Fatal(offer)
				}
				h.reply(id, "accept")
			}
			h.ready(id, "name")
			h.reply(id, "name")
			if scenario == "clarification" {
				h.until(id, func(s map[string]any) bool {
					return s["stage"] == "name" && s["replyBusy"] == false && len(s["messages"].([]any)) >= 5
				})
				h.reply(id, "name")
			}
			details := h.ready(id, "details")
			if details["employeeConfirmed"] == true {
				t.Fatal("Details question is not employee acceptance")
			}
			h.reply(id, "confirm_details")
			confirmed := h.ready(id, "confirmed")
			reservation := confirmed["reservation"].(map[string]any)
			if reservation["status"] != "awaiting_confirmation" {
				t.Fatal(reservation)
			}
			code, result := h.request("/api/calls/"+id+"/confirm", map[string]any{"expected_reservation_revision": reservation["revision"]}, "")
			if code != 200 || result["reservation"].(map[string]any)["status"] != "confirmed" {
				t.Fatal(code, result)
			}
			metrics := result["metrics"].(map[string]any)
			if metrics["outboundFrames"].(float64) < 3 || metrics["marks"].(float64) < 3 {
				t.Fatal("Actual media/mark messages were not exchanged", metrics)
			}
			h.reply(id, "thanks")
			h.until(id, func(s map[string]any) bool { return s["status"] == "completed" })
		})
	}
}
func TestFailureScenariosDoNotConfirmReservation(t *testing.T) {
	for _, scenario := range []string{"busy", "no_answer", "fully_booked", "voicemail", "disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t)
			id := h.create(scenario)
			if scenario == "fully_booked" || scenario == "disconnect" {
				h.ready(id, "greeting")
				h.reply(id, "request")
			}
			s := h.until(id, func(s map[string]any) bool { return s["stage"] == "unavailable" || terminal(s["status"].(string)) })
			if s["employeeConfirmed"] == true || s["reservation"].(map[string]any)["status"] == "confirmed" {
				t.Fatal("Failure implied reservation", s)
			}
		})
	}
}
func TestCreateIdempotencyAndEarlyHangup(t *testing.T) {
	h := newHarness(t)
	r := DefaultReservation()
	key := uuid()
	ids := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, b := h.request("/api/calls", r, key)
			if code != 201 {
				t.Error(code, b)
				return
			}
			ids <- b["id"].(string)
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for next := range ids {
		if id != "" && next != id {
			t.Fatal("Retry created second call")
		}
		id = next
	}
	h.mock.mu.Lock()
	count := len(h.mock.calls)
	h.mock.mu.Unlock()
	if count != 1 {
		t.Fatal(count)
	}
	r.Name = "다른 이름"
	if status, _ := h.request("/api/calls", r, key); status != 409 {
		t.Fatal("Key reuse accepted")
	}
	if status, _ := h.request("/api/calls/"+id+"/end", map[string]any{}, ""); status != 200 {
		t.Fatal(status)
	}
	time.Sleep(40 * time.Millisecond)
	_, s := h.request("/api/calls/"+id, nil, "")
	if s["status"] != "completed" {
		t.Fatal("Hangup later became connected", s)
	}
}
func TestStoppedReplyIsNotDeliveredOnClearMark(t *testing.T) {
	h := newHarness(t)
	id := h.create("available")
	h.ready(id, "greeting")
	s := h.app.get(id)
	s.mu.Lock()
	if e := h.app.sendLocked(s, "media", map[string]any{"media": map[string]string{"payload": strings.Repeat("////", 800)}}); e != nil {
		t.Fatal(e)
	}
	s.mu.Unlock()
	h.reply(id, "request")
	code, _ := h.request("/api/calls/"+id+"/stop", map[string]any{}, "")
	if code != 200 {
		t.Fatal(code)
	}
	time.Sleep(60 * time.Millisecond)
	_, result := h.request("/api/calls/"+id, nil, "")
	if result["stage"] != "greeting" {
		t.Fatal("Canceled reply reached employee", result)
	}
	messages := result["messages"].([]any)
	if messages[len(messages)-1].(map[string]any)["status"] != "cancelled" {
		t.Fatal(messages)
	}
}
func TestProtocolAuthSignatureAndExternalDestinations(t *testing.T) {
	h := newHarness(t)
	response, e := http.Get(h.mockServer.URL + "/" + APIVersion + "/Accounts/" + AccountSID + "/Calls.json")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal(response.StatusCode)
	}
	values := url.Values{"To": {"+81312345678"}, "From": {"+81300000000"}, "Url": {"https://example.com/voice"}}
	r, _ := http.NewRequest("POST", h.mockServer.URL+"/"+APIVersion+"/Accounts/"+AccountSID+"/Calls.json", strings.NewReader(values.Encode()))
	r.SetBasicAuth(AccountSID, AuthToken)
	response, e = http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("External webhook accepted")
	}
	v := url.Values{"z": {"2", "1", "1"}, "a": {"test"}}
	signature := Sign("http://localhost/voice", v, AuthToken)
	if !Verify("http://localhost/voice", url.Values{"a": {"test"}, "z": {"1", "2"}}, signature, AuthToken) || Verify("http://localhost/other", v, signature, AuthToken) {
		t.Fatal("Signature validation")
	}
	for _, payload := range []string{"UklGRg==", "", "Uk lGRg=="} {
		if _, e := PCMU(payload); e == nil {
			t.Fatal(fmt.Sprintf("Invalid PCMU accepted: %q", payload))
		}
	}
}
func TestFactChangesInvalidateApprovalAndRejectStaleRevision(t *testing.T) {
	h := newHarness(t)
	id := h.create("available")
	h.ready(id, "greeting")
	s := h.app.get(id)
	for _, change := range []map[string]any{{"date": "2027-01-11"}, {"partySize": 4}, {"name": "이민수"}, {"nameJa": "イ・ミンス"}, {"price": 5000}, {"cancellationTerms": "당일 취소 전액"}} {
		s.mu.Lock()
		s.confirmed = true
		s.r.EmployeeRevision = s.r.Revision
		s.r.UserRevision = s.r.Revision
		s.r.Status = "confirmed"
		oldRevision := s.r.Revision
		clock := s.r.ProposedTime
		sid := s.callSID
		s.mu.Unlock()
		h.app.fixture(sid, Fixture{JA: "変更があります。", KO: "변경 사항이 있습니다.", Stage: "details", ProposedTime: clock, Facts: change})
		s.mu.Lock()
		if s.confirmed || s.r.UserRevision != 0 || s.r.EmployeeRevision != 0 || s.r.Revision != oldRevision+1 {
			s.mu.Unlock()
			t.Fatal("Fact change inherited approval", change)
		}
		s.mu.Unlock()
		status, _ := h.request("/api/calls/"+id+"/confirm", map[string]any{"expected_reservation_revision": oldRevision}, "")
		if status != 409 {
			t.Fatal("Stale approval accepted", change)
		}
	}
}
