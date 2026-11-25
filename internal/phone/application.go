package phone

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Message struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Original   string `json:"original"`
	Translated string `json:"translated"`
	Status     string `json:"status,omitempty"`
	At         string `json:"at"`
}
type session struct {
	mu                               sync.Mutex
	id, callSID, status, stage       string
	r                                Reservation
	messages                         []*Message
	revision, callbackSeq            int
	confirmed, busy, playbackFailure bool
	pending                          *Message
	socket                           *websocket.Conn
	stream                           string
	subscribers                      map[chan struct{}]bool
}
type command struct {
	fingerprint string
	done        chan struct{}
	id          string
	err         error
}
type Application struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	mu                  sync.Mutex
	root, base, mockURL string
	mock                *Mock
	sessions            map[string]*session
	commands            map[string]*command
	closed              bool
}

func NewApplication(ctx context.Context, root, base, mockURL string, mock *Mock) *Application {
	ctx, cancel := context.WithCancel(ctx)
	a := &Application{ctx: ctx, cancel: cancel, root: root, base: base, mockURL: mockURL, mock: mock, sessions: map[string]*session{}, commands: map[string]*command{}}
	if mock != nil {
		mock.Fixture = a.fixture
	}
	return a
}
func uuid() string {
	x := sid("")
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func (a *Application) get(id string) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}
func (s *session) publish() {
	s.revision++
	for c := range s.subscribers {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}
func (s *session) invalidate() {
	s.confirmed = false
	s.r.EmployeeRevision = 0
	s.r.UserRevision = 0
	s.r.ConfirmedFacts = nil
}

var labels = map[string]string{"queued": "연결 준비 중", "initiated": "전화 거는 중", "ringing": "신호 보내는 중", "in-progress": "통화 연결됨", "completed": "통화 종료", "busy": "통화 중", "no-answer": "응답 없음", "failed": "연결 실패", "canceled": "통화 취소됨"}

func (a *Application) snapshot(s *session) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := []Message{}
	for _, m := range s.messages {
		messages = append(messages, *m)
	}
	options := []Phrase{}
	if s.status == "in-progress" && !s.busy && !s.playbackFailure {
		options = suggestions(s.r, s.stage)
	}
	metrics := any(Metrics{})
	trace := any([]Trace{})
	if a.mock != nil {
		if d := a.mock.Inspect(s.callSID); d != nil {
			metrics = d["metrics"]
			trace = d["events"]
		}
	}
	return map[string]any{"id": s.id, "callSid": s.callSID, "revision": s.revision, "status": s.status, "statusLabel": labels[s.status], "reservation": s.r, "stage": s.stage, "messages": messages, "replyBusy": s.busy, "pendingReply": s.pending != nil, "employeeConfirmed": s.confirmed, "suggestions": options, "metrics": metrics, "trace": trace, "scenario": s.r.Scenario}
}
func (a *Application) request(ctx context.Context, path string, values url.Values) (Resource, error) {
	if err := Loopback(a.mockURL, false); err != nil {
		return Resource{}, err
	}
	method := "GET"
	var body io.Reader
	if values != nil {
		method = "POST"
		body = strings.NewReader(values.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, a.mockURL+"/"+APIVersion+"/Accounts/"+AccountSID+"/"+path, body)
	if e != nil {
		return Resource{}, e
	}
	req.SetBasicAuth(AccountSID, AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, e := httpClient.Do(req)
	if e != nil {
		return Resource{}, e
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Resource{}, fmt.Errorf("Mock HTTP %d", response.StatusCode)
	}
	var resource Resource
	e = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&resource)
	return resource, e
}
func (a *Application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := r.URL.Path
	if r.Method == "GET" {
		assets := map[string]string{"/": "index.html", "/app.js": "app.js", "/styles.css": "styles.css", "/favicon.svg": "favicon.svg"}
		if asset, ok := assets[path]; ok {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
			http.ServeFile(w, r, filepath.Join(a.root, "public", asset))
			return
		}
		if path == "/api/config" {
			reply(w, 200, map[string]any{"scenarios": Scenarios, "mode": "twilio-local-mock", "userLanguage": "ko-KR", "shopLanguage": "ja-JP", "engine": "go"})
			return
		}
	}
	if r.Method == "POST" && strings.HasPrefix(path, "/api/") && r.Header.Get("Origin") != "" && r.Header.Get("Origin") != a.base {
		fail(w, 403, "Use the same origin")
		return
	}
	if r.Method == "POST" && path == "/api/calls" {
		a.create(w, r)
		return
	}
	if strings.HasPrefix(path, "/webhooks/twilio/") && r.Method == "POST" {
		a.webhook(w, r)
		return
	}
	if strings.HasPrefix(path, "/media/") {
		a.socket(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/calls/"), "/")
	if strings.HasPrefix(path, "/api/calls/") && len(parts) <= 2 {
		s := a.get(parts[0])
		if s == nil {
			fail(w, 404, "Call not found")
			return
		}
		if r.Method == "GET" {
			if len(parts) == 1 {
				reply(w, 200, a.snapshot(s))
				return
			}
			if parts[1] == "events" {
				a.events(w, r, s)
				return
			}
		}
		if r.Method == "POST" && len(parts) == 2 {
			a.action(w, r, s, parts[1])
			return
		}
	}
	fail(w, 404, "Not found")
}
func (a *Application) create(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if decode(w, r, &raw) != nil {
		fail(w, 400, "Invalid reservation")
		return
	}
	var reservation Reservation
	b, _ := json.Marshal(raw)
	party := raw["partySize"]
	delete(raw, "partySize")
	b, _ = json.Marshal(raw)
	if json.Unmarshal(b, &reservation) != nil {
		fail(w, 400, "Invalid reservation")
		return
	}
	if json.Unmarshal(party, &reservation.PartySize) != nil {
		var value string
		_ = json.Unmarshal(party, &value)
		reservation.PartySize, _ = strconv.Atoi(value)
	}
	if e := reservation.Validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	// 승인 여부와 수정 버전은 서버가 관리하므로 클라이언트가 보낸 값은 무시한다.
	reservation = Reservation{Name: reservation.Name, NameJA: reservation.NameJA, Date: reservation.Date, Time: reservation.Time, PartySize: reservation.PartySize, Phone: reservation.Phone, Scenario: reservation.Scenario}
	key := r.Header.Get("Idempotency-Key")
	if !regexp.MustCompile(`^[A-Za-z0-9:_-]{8,128}$`).MatchString(key) {
		fail(w, 400, "Idempotency-Key is required")
		return
	}
	b, _ = json.Marshal(reservation)
	fingerprint := string(b)
	a.mu.Lock()
	cmd := a.commands[key]
	if cmd != nil && cmd.fingerprint != fingerprint {
		a.mu.Unlock()
		fail(w, 409, "Request key was used with different facts")
		return
	}
	if cmd == nil {
		if a.closed || len(a.commands) >= 100 {
			a.mu.Unlock()
			fail(w, 429, "Session capacity reached")
			return
		}
		cmd = &command{fingerprint: fingerprint, done: make(chan struct{})}
		a.commands[key] = cmd
		id := uuid()
		s := &session{id: id, status: "queued", r: reservation, callbackSeq: -1, messages: []*Message{}, subscribers: map[chan struct{}]bool{}}
		s.r.Revision = 1
		s.r.Status = "draft"
		a.sessions[id] = s
		cmd.id = id
		go func() {
			defer close(cmd.done)
			if a.mock != nil {
				if e := a.mock.Prepare(Plan{To: reservation.Phone, Scenario: reservation.Scenario, Reservation: reservation}); e != nil {
					cmd.err = e
					return
				}
			}
			values := url.Values{"To": {reservation.Phone}, "From": {"+81300000000"}, "Url": {a.base + "/webhooks/twilio/voice/" + id}, "StatusCallback": {a.base + "/webhooks/twilio/status/" + id}, "StatusCallbackEvent": {"initiated", "ringing", "answered", "completed"}}
			resource, e := a.request(a.ctx, "Calls.json", values)
			if e != nil {
				cmd.err = e
				return
			}
			s.mu.Lock()
			s.callSID = resource.SID
			s.mu.Unlock()
		}()
	}
	a.mu.Unlock()
	select {
	case <-r.Context().Done():
		return
	case <-cmd.done:
	}
	if cmd.err != nil {
		fail(w, 503, cmd.err.Error())
		return
	}
	reply(w, 201, a.snapshot(a.get(cmd.id)))
}
func (a *Application) events(w http.ResponseWriter, r *http.Request, s *session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "Streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if len(s.subscribers) >= 8 {
		s.mu.Unlock()
		fail(w, 429, "Too many event listeners")
		return
	}
	s.subscribers[ch] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, ch); s.mu.Unlock() }()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	send := func() bool {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		b, _ := json.Marshal(a.snapshot(s))
		_, e := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
		flusher.Flush()
		return e == nil
	}
	if !send() {
		return
	}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
func (a *Application) action(w http.ResponseWriter, r *http.Request, s *session, action string) {
	var body struct {
		Text     string `json:"text"`
		Revision int    `json:"expected_reservation_revision"`
	}
	if decode(w, r, &body) != nil {
		fail(w, 400, "Invalid JSON")
		return
	}
	if action == "end" {
		s.mu.Lock()
		id := s.callSID
		s.mu.Unlock()
		if _, e := a.request(r.Context(), "Calls/"+id+".json", url.Values{"Status": {"completed"}}); e != nil {
			fail(w, 503, e.Error())
			return
		}
		reply(w, 200, a.snapshot(s))
		return
	}
	s.mu.Lock()
	if (action == "replies" || action == "confirm") && body.Revision != s.r.Revision {
		s.mu.Unlock()
		fail(w, 409, "예약 내용이 변경되었습니다. 최신 내용을 확인해 주세요.")
		return
	}
	code := 200
	switch action {
	case "replies":
		if s.status != "in-progress" || s.busy || s.playbackFailure {
			s.mu.Unlock()
			fail(w, 409, "Wait for the employee and playback")
			return
		}
		var selected *Phrase
		for _, p := range suggestions(s.r, s.stage) {
			if strings.Join(strings.Fields(body.Text), " ") == p.KO {
				p := p
				selected = &p
				break
			}
		}
		if selected == nil {
			s.mu.Unlock()
			fail(w, 422, "Choose a prepared sentence")
			return
		}
		message := &Message{ID: uuid(), Role: "user", Original: selected.KO, Translated: selected.JA, Status: "sending", At: time.Now().UTC().Format(time.RFC3339Nano)}
		s.pending = message
		s.busy = true
		s.messages = append(s.messages, message)
		if e := a.sendLocked(s, "media", map[string]any{"media": map[string]string{"payload": Silence()}}); e != nil {
			message.Status = "failed"
			s.pending = nil
			s.busy = false
			s.mu.Unlock()
			fail(w, 409, e.Error())
			return
		}
		if e := a.sendLocked(s, "mark", map[string]any{"mark": map[string]string{"name": message.ID}}); e != nil {
			message.Status = "failed"
			s.pending = nil
			s.busy = false
			s.playbackFailure = true
			s.publish()
			s.mu.Unlock()
			fail(w, 409, e.Error())
			return
		}
		code = 202
	case "stop":
		if s.pending != nil {
			s.pending.Status = "cancelled"
			s.pending = nil
			s.busy = false
		}
		if s.status == "in-progress" {
			if e := a.sendLocked(s, "clear", nil); e != nil {
				s.playbackFailure = true
				s.invalidate()
				s.r.Status = "uncertain"
			}
		}
	case "confirm":
		if !s.confirmed || s.r.EmployeeRevision != s.r.Revision || s.r.Status == "unavailable" || s.r.Status == "cancelled" || s.r.Status == "uncertain" {
			s.mu.Unlock()
			fail(w, 409, "Employee must confirm the current facts first")
			return
		}
		s.r.UserRevision = s.r.Revision
		s.r.Status = "confirmed"
		s.r.ConfirmedFacts = facts(s.r)
	default:
		s.mu.Unlock()
		fail(w, 404, "Unknown call action")
		return
	}
	s.publish()
	s.mu.Unlock()
	reply(w, code, a.snapshot(s))
}
func (a *Application) webhook(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/webhooks/twilio/"), "/")
	if len(parts) != 2 {
		fail(w, 404, "Not found")
		return
	}
	s := a.get(parts[1])
	if s == nil {
		fail(w, 404, "Session not found")
		return
	}
	values, e := form(w, r)
	if e != nil || !Verify(a.base+r.URL.RequestURI(), values, r.Header.Get("X-Twilio-Signature"), AuthToken) || values.Get("AccountSid") != AccountSID {
		fail(w, 403, "Invalid signature")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callSID != "" && s.callSID != values.Get("CallSid") {
		fail(w, 403, "Call SID mismatch")
		return
	}
	s.callSID = values.Get("CallSid")
	if parts[0] == "voice" {
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(strings.Replace(a.base, "http", "ws", 1)+"/media/"+s.id))
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<Response><Connect><Stream url="%s"><Parameter name="sessionId" value="%s"/></Stream></Connect></Response>`, escaped.String(), s.id)
		return
	}
	sequence, e := strconv.Atoi(values.Get("SequenceNumber"))
	status := values.Get("CallStatus")
	if parts[0] != "status" || e != nil || sequence < 0 || labels[status] == "" {
		fail(w, 400, "Invalid callback")
		return
	}
	if sequence > s.callbackSeq && !terminal(s.status) {
		s.callbackSeq = sequence
		s.status = status
		if status == "in-progress" {
			s.r.Status = "discussing"
		}
		if terminal(status) {
			if s.pending != nil {
				s.pending.Status = "cancelled"
			}
			s.pending = nil
			s.busy = false
			if s.r.Status != "confirmed" && s.r.Status != "unavailable" && s.r.Status != "cancelled" && !s.confirmed {
				s.r.Status = "uncertain"
			}
		}
		s.publish()
	}
	w.WriteHeader(204)
}
func (a *Application) fixture(id string, f Fixture) {
	a.mu.Lock()
	var s *session
	for _, candidate := range a.sessions {
		candidate.mu.Lock()
		match := candidate.callSID == id
		candidate.mu.Unlock()
		if match {
			s = candidate
			break
		}
	}
	a.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if terminal(s.status) {
		return
	}
	s.messages = append(s.messages, &Message{ID: uuid(), Role: "employee", Original: f.JA, Translated: f.KO, At: time.Now().UTC().Format(time.RFC3339Nano)})
	s.stage = f.Stage
	s.busy = false
	before, _ := json.Marshal(facts(s.r))
	s.r.ProposedTime = f.ProposedTime
	for key, value := range f.Facts {
		switch key {
		case "date":
			if v, ok := value.(string); ok {
				s.r.Date = v
			}
		case "name":
			if v, ok := value.(string); ok {
				s.r.Name = v
			}
		case "nameJa":
			if v, ok := value.(string); ok {
				s.r.NameJA = v
			}
		case "partySize":
			switch v := value.(type) {
			case int:
				s.r.PartySize = v
			case float64:
				s.r.PartySize = int(v)
			}
		case "price":
			s.r.Price = value
		case "cancellationTerms":
			s.r.CancellationTerms = value
		}
	}
	after, _ := json.Marshal(facts(s.r))
	if string(before) != string(after) {
		s.r.Revision++
		s.invalidate()
		s.r.Status = "discussing"
		if s.pending != nil {
			s.pending.Status = "cancelled"
			s.pending = nil
			if a.sendLocked(s, "clear", nil) != nil {
				s.playbackFailure = true
			}
		}
	}
	switch f.Stage {
	case "offer":
		s.r.Status = "proposed"
	case "name":
		s.r.Status = "discussing"
	case "details":
		s.r.Status = "reviewing"
	}
	if f.Outcome != "" {
		s.invalidate()
		s.r.Status = f.Outcome
		if f.Outcome == "voicemail" {
			s.r.Status = "uncertain"
		}
	}
	if f.Confirmed && f.Outcome == "" {
		s.confirmed = true
		s.r.EmployeeRevision = s.r.Revision
		s.r.Status = "awaiting_confirmation"
	}
	if s.playbackFailure {
		s.invalidate()
		s.r.Status = "uncertain"
	}
	s.publish()
}
func facts(r Reservation) map[string]any {
	clock := r.ProposedTime
	if clock == "" {
		clock = r.Time
	}
	return map[string]any{"date": r.Date, "time": clock, "partySize": r.PartySize, "name": r.Name, "nameJa": r.NameJA, "price": r.Price, "cancellationTerms": r.CancellationTerms}
}
func (a *Application) sendLocked(s *session, event string, extra map[string]any) error {
	if s.socket == nil {
		return errors.New("No connected stream")
	}
	body := map[string]any{"event": event, "streamSid": s.stream}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	return s.socket.Write(ctx, websocket.MessageText, b)
}
func (a *Application) socket(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/media/")
	s := a.get(id)
	if s == nil || r.Method != "GET" || !Verify(a.base+r.URL.RequestURI(), nil, r.Header.Get("X-Twilio-Signature"), AuthToken) {
		fail(w, 403, "Invalid stream signature")
		return
	}
	conn, e := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if e != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(128 * 1024)
	defer func() {
		s.mu.Lock()
		if s.socket == conn {
			s.socket = nil
		}
		s.mu.Unlock()
	}()
	for {
		_, data, e := conn.Read(a.ctx)
		if e != nil {
			return
		}
		var event struct {
			Event, StreamSID string
			Start            struct {
				AccountSID, CallSID string
				Custom              map[string]string `json:"customParameters"`
				Format              struct {
					Encoding string
					Rate     int `json:"sampleRate"`
					Channels int
				} `json:"mediaFormat"`
			}
			Mark struct{ Name string }
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		s.mu.Lock()
		switch event.Event {
		case "start":
			if event.Start.AccountSID != AccountSID || event.Start.CallSID != s.callSID || event.Start.Custom["sessionId"] != s.id || event.Start.Format.Encoding != "audio/x-mulaw" || event.Start.Format.Rate != 8000 || event.Start.Format.Channels != 1 || terminal(s.status) || s.socket != nil {
				s.mu.Unlock()
				return
			}
			s.socket = conn
			s.stream = event.StreamSID
		case "mark":
			if s.socket != conn || event.StreamSID != s.stream {
				s.mu.Unlock()
				return
			}
			if s.pending != nil && s.pending.ID == event.Mark.Name {
				pending := s.pending
				s.pending = nil
				pending.Status = "delivered"
				if a.mock != nil {
					if e := a.mock.Feed(s.callSID, pending.Translated); e != nil {
						pending.Status = "failed"
						s.busy = false
						s.playbackFailure = true
						s.invalidate()
						s.r.Status = "uncertain"
					}
				}
			}
		case "stop":
			s.mu.Unlock()
			return
		}
		s.publish()
		s.mu.Unlock()
	}
}
func (a *Application) Close() {
	a.cancel()
	a.mu.Lock()
	a.closed = true
	for _, s := range a.sessions {
		s.mu.Lock()
		if s.socket != nil {
			s.socket.CloseNow()
		}
		s.mu.Unlock()
	}
	a.mu.Unlock()
}
