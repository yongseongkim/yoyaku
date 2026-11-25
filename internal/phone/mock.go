package phone

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Resource struct {
	SID       string `json:"sid"`
	Account   string `json:"account_sid"`
	To        string `json:"to"`
	From      string `json:"from"`
	Status    string `json:"status"`
	Direction string `json:"direction"`
	Version   string `json:"api_version"`
	URI       string `json:"uri"`
	Created   string `json:"date_created"`
	Updated   string `json:"date_updated"`
	Start     string `json:"start_time"`
	End       string `json:"end_time"`
	Duration  string `json:"duration"`
}
type Metrics struct {
	Inbound  int `json:"inboundFrames"`
	Outbound int `json:"outboundFrames"`
	Bytes    int `json:"outboundBytes"`
	Marks    int `json:"marks"`
	Clears   int `json:"clears"`
}
type Trace struct {
	At        string `json:"at"`
	Direction string `json:"direction"`
	Event     string `json:"event"`
	Detail    string `json:"detail"`
}
type Plan struct {
	To          string      `json:"to"`
	Scenario    string      `json:"scenario"`
	Reservation Reservation `json:"reservation"`
}
type call struct {
	mu                      sync.Mutex
	resource                Resource
	params                  url.Values
	plan                    Plan
	events                  []Trace
	metrics                 Metrics
	ctx                     context.Context
	cancel                  context.CancelFunc
	socket                  *websocket.Conn
	stream                  string
	seq, callbackSeq, chunk int
	timestamp               int
	playUntil               time.Time
	marks                   map[string]context.CancelFunc
	frames                  chan []byte
	shop                    *Shop
	commands                chan string
}
type Mock struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	calls   map[string]*call
	plans   map[string][]Plan
	Delay   time.Duration
	Fixture func(string, Fixture)
	wg      sync.WaitGroup
	closed  bool
}

func NewMock(ctx context.Context) *Mock {
	ctx, cancel := context.WithCancel(ctx)
	return &Mock{ctx: ctx, cancel: cancel, calls: map[string]*call{}, plans: map[string][]Plan{}, Delay: 350 * time.Millisecond}
}
func (m *Mock) Prepare(p Plan) error {
	if !validScenario(p.Scenario) || !regexp.MustCompile(`^\+[1-9]\d{7,14}$`).MatchString(p.To) {
		return errors.New("Invalid mock plan")
	}
	if p.Reservation.Date == "" {
		p.Reservation = DefaultReservation()
	}
	p.Reservation.Scenario = p.Scenario
	if e := p.Reservation.Validate(); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.plans[p.To]) >= 100 || m.closed {
		return errors.New("Mock plan capacity reached")
	}
	m.plans[p.To] = append(m.plans[p.To], p)
	return nil
}
func (c *call) log(direction, event, detail string) {
	c.events = append(c.events, Trace{time.Now().UTC().Format(time.RFC3339Nano), direction, event, detail})
	if len(c.events) > 400 {
		c.events = c.events[len(c.events)-400:]
	}
}
func (c *call) snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"call": c.resource, "metrics": c.metrics, "events": append([]Trace{}, c.events...), "scenario": c.plan.Scenario}
}
func (c *call) resourceCopy() Resource { c.mu.Lock(); defer c.mu.Unlock(); return c.resource }
func (m *Mock) get(id string) *call    { m.mu.Lock(); defer m.mu.Unlock(); return m.calls[id] }
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/health" {
		reply(w, 200, map[string]any{"mode": "local-twilio-mock", "realCalls": false, "engine": "go"})
		return
	}
	user, password, ok := r.BasicAuth()
	if !ok || user != AccountSID || password != AuthToken {
		fail(w, 401, "Authenticate")
		return
	}
	if r.Method == "POST" && r.Header.Get("Origin") != "" {
		fail(w, 403, "Use the mock adapter")
		return
	}
	if path == "/__mock/scenarios" && r.Method == "GET" {
		reply(w, 200, Scenarios)
		return
	}
	if path == "/__mock/next-call" && r.Method == "POST" {
		var p Plan
		if decode(w, r, &p) != nil {
			fail(w, 400, "Invalid plan")
			return
		}
		if e := m.Prepare(p); e != nil {
			fail(w, 400, e.Error())
			return
		}
		reply(w, 201, map[string]bool{"queued": true})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/__mock/calls/"), "/")
	if strings.HasPrefix(path, "/__mock/calls/") && len(parts) <= 2 {
		c := m.get(parts[0])
		if c == nil {
			fail(w, 404, "Call not found")
			return
		}
		if len(parts) == 1 && r.Method == "GET" {
			reply(w, 200, c.snapshot())
			return
		}
		if r.Method == "POST" && len(parts) == 2 {
			var body struct {
				Payload  string `json:"payload"`
				Japanese string `json:"japanese"`
			}
			if decode(w, r, &body) != nil {
				fail(w, 400, "Invalid JSON")
				return
			}
			var e error
			switch parts[1] {
			case "media":
				var data []byte
				data, e = PCMU(body.Payload)
				if e == nil {
					e = m.inject(c, data)
				}
			case "employee-input":
				e = m.Feed(parts[0], body.Japanese)
			default:
				fail(w, 404, "Not found")
				return
			}
			if e != nil {
				fail(w, 409, e.Error())
				return
			}
			reply(w, 202, map[string]bool{"accepted": true})
			return
		}
	}
	collection := "/" + APIVersion + "/Accounts/" + AccountSID + "/Calls"
	if path == collection+".json" {
		if r.Method == "GET" {
			m.mu.Lock()
			calls := []Resource{}
			for _, c := range m.calls {
				calls = append(calls, c.resourceCopy())
			}
			m.mu.Unlock()
			reply(w, 200, map[string]any{"calls": calls, "next_page_uri": nil, "previous_page_uri": nil, "page": 0})
			return
		}
		if r.Method == "POST" {
			values, e := form(w, r)
			if e != nil {
				fail(w, 400, e.Error())
				return
			}
			c, e := m.create(values)
			if e != nil {
				fail(w, 400, e.Error())
				return
			}
			reply(w, 201, c.resourceCopy())
			return
		}
	}
	if strings.HasPrefix(path, collection+"/") && strings.HasSuffix(path, ".json") {
		id := strings.TrimSuffix(strings.TrimPrefix(path, collection+"/"), ".json")
		c := m.get(id)
		if c == nil {
			fail(w, 404, "Call not found")
			return
		}
		if r.Method == "GET" {
			reply(w, 200, c.resourceCopy())
			return
		}
		if r.Method == "POST" {
			v, e := form(w, r)
			if e != nil || v.Get("Status") != "completed" && v.Get("Status") != "canceled" {
				fail(w, 400, "Use completed or canceled status")
				return
			}
			m.finish(c, v.Get("Status"))
			reply(w, 200, c.resourceCopy())
			return
		}
	}
	fail(w, 404, "Unsupported mock endpoint")
}
func (m *Mock) create(v url.Values) (*call, error) {
	for _, key := range []string{"To", "From"} {
		if !regexp.MustCompile(`^\+[1-9]\d{7,14}$`).MatchString(v.Get(key)) {
			return nil, fmt.Errorf("Invalid %s number", key)
		}
	}
	if v.Get("Url") == "" && v.Get("Twiml") == "" {
		return nil, errors.New("Url or Twiml is required")
	}
	for _, k := range []string{"Url", "StatusCallback"} {
		if v.Get(k) != "" {
			if e := Loopback(v.Get(k), false); e != nil {
				return nil, e
			}
		}
	}
	for _, k := range []string{"Method", "StatusCallbackMethod"} {
		if v.Get(k) != "" && v.Get(k) != "POST" {
			return nil, errors.New("Only POST webhooks supported")
		}
	}
	for _, event := range v["StatusCallbackEvent"] {
		if event != "initiated" && event != "ringing" && event != "answered" && event != "completed" {
			return nil, errors.New("Unsupported callback event")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.calls) >= 100 {
		return nil, errors.New("Mock call capacity reached")
	}
	p := Plan{Scenario: "available", Reservation: DefaultReservation()}
	if plans := m.plans[v.Get("To")]; len(plans) > 0 {
		p = plans[0]
		m.plans[v.Get("To")] = plans[1:]
	}
	id := sid("CA")
	now := time.Now().UTC().Format(http.TimeFormat)
	ctx, cancel := context.WithCancel(m.ctx)
	c := &call{resource: Resource{SID: id, Account: AccountSID, To: v.Get("To"), From: v.Get("From"), Status: "queued", Direction: "outbound-api", Version: APIVersion, URI: "/" + APIVersion + "/Accounts/" + AccountSID + "/Calls/" + id + ".json", Created: now, Updated: now}, params: v, plan: p, ctx: ctx, cancel: cancel, marks: map[string]context.CancelFunc{}, frames: make(chan []byte, 300), commands: make(chan string, 8)}
	c.log("app -> mock", "POST Calls.json", v.Get("To"))
	m.calls[id] = c
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.run(c) }()
	return c, nil
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (m *Mock) callback(c *call, event, status string) {
	c.mu.Lock()
	if terminal(c.resource.Status) && !terminal(status) {
		c.mu.Unlock()
		return
	}
	c.resource.Status = status
	c.resource.Updated = time.Now().UTC().Format(http.TimeFormat)
	c.log("mock -> app", "call:"+status, "")
	seq := c.callbackSeq
	c.callbackSeq++
	rawURL := c.params.Get("StatusCallback")
	selected := len(c.params["StatusCallbackEvent"]) == 0
	for _, e := range c.params["StatusCallbackEvent"] {
		selected = selected || e == event
	}
	resource := c.resource
	c.mu.Unlock()
	if rawURL == "" || !selected {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
	defer cancel()
	_, _ = post(ctx, rawURL, url.Values{"AccountSid": {AccountSID}, "CallSid": {resource.SID}, "CallStatus": {status}, "SequenceNumber": {fmt.Sprint(seq)}, "To": {resource.To}, "From": {resource.From}}, AuthToken)
}
func (m *Mock) run(c *call) {
	defer func() {
		if !terminal(c.resourceCopy().Status) {
			m.finish(c, "failed")
		}
	}()
	for _, item := range []struct{ event, status string }{{"initiated", "initiated"}, {"ringing", "ringing"}} {
		if !wait(c.ctx, m.Delay) {
			return
		}
		m.callback(c, item.event, item.status)
	}
	if !wait(c.ctx, m.Delay) {
		return
	}
	switch c.plan.Scenario {
	case "busy":
		m.finish(c, "busy")
		return
	case "no_answer":
		m.finish(c, "no-answer")
		return
	}
	c.mu.Lock()
	c.resource.Start = time.Now().UTC().Format(http.TimeFormat)
	c.mu.Unlock()
	m.callback(c, "answered", "in-progress")
	if c.ctx.Err() != nil {
		return
	}
	twiml := []byte(c.params.Get("Twiml"))
	var e error
	if len(twiml) == 0 {
		twiml, e = post(c.ctx, c.params.Get("Url"), url.Values{"AccountSid": {AccountSID}, "CallSid": {c.resource.SID}, "CallStatus": {"in-progress"}}, AuthToken)
		if e != nil {
			return
		}
	}
	var response struct {
		Connect struct {
			Stream struct {
				URL        string `xml:"url,attr"`
				Parameters []struct {
					Name  string `xml:"name,attr"`
					Value string `xml:"value,attr"`
				} `xml:"Parameter"`
			} `xml:"Stream"`
		} `xml:"Connect"`
	}
	if xml.Unmarshal(twiml, &response) != nil {
		return
	}
	stream := response.Connect.Stream
	if Loopback(stream.URL, true) != nil {
		return
	}
	headers := http.Header{"X-Twilio-Signature": {Sign(strings.Replace(stream.URL, "ws", "http", 1), nil, AuthToken)}}
	conn, _, e := websocket.Dial(c.ctx, stream.URL, &websocket.DialOptions{HTTPHeader: headers, HTTPClient: httpClient})
	if e != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(128 * 1024)
	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	c.socket = conn
	c.stream = sid("MZ")
	streamID := c.stream
	c.mu.Unlock()
	custom := map[string]string{}
	for _, p := range stream.Parameters {
		custom[p.Name] = p.Value
	}
	m.send(c, "connected", map[string]any{"protocol": "Call", "version": "1.0.0"})
	m.send(c, "start", map[string]any{"start": map[string]any{"accountSid": AccountSID, "callSid": c.resource.SID, "streamSid": streamID, "tracks": []string{"inbound"}, "mediaFormat": map[string]any{"encoding": "audio/x-mulaw", "sampleRate": 8000, "channels": 1}, "customParameters": custom}})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, data, e := conn.Read(c.ctx)
			if e != nil {
				return
			}
			if m.outgoing(c, data) != nil {
				return
			}
		}
	}()
	defer func() { c.cancel(); conn.CloseNow(); <-readDone }()
	c.shop = &Shop{R: c.plan.Reservation}
	fixture, finish := c.shop.Start()
	m.emit(c, fixture)
	if finish {
		if wait(c.ctx, m.Delay*3) {
			m.finish(c, "completed")
		}
		return
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-readDone:
			m.finish(c, "completed")
			return
		case <-ticker.C:
			select {
			case data := <-c.frames:
				c.mu.Lock()
				c.chunk++
				chunk, timestamp := c.chunk, c.timestamp
				c.timestamp += len(data) / 8
				c.metrics.Inbound++
				c.mu.Unlock()
				m.send(c, "media", map[string]any{"media": map[string]string{"track": "inbound", "chunk": fmt.Sprint(chunk), "timestamp": fmt.Sprint(timestamp), "payload": base64.StdEncoding.EncodeToString(data)}})
			default:
			}
		case ja := <-c.commands:
			if !wait(c.ctx, m.Delay) {
				return
			}
			f, end := c.shop.Receive(ja)
			if f.JA != "" {
				m.emit(c, f)
			}
			if end {
				if wait(c.ctx, m.Delay*2) {
					m.finish(c, "completed")
				}
				return
			}
		}
	}
}
func (m *Mock) emit(c *call, f Fixture) {
	frame := make([]byte, 160)
	for i := range frame {
		frame[i] = 255
	}
	_ = m.inject(c, frame)
	if m.Fixture != nil {
		m.Fixture(c.resource.SID, f)
	}
}
func (m *Mock) send(c *call, event string, extra map[string]any) {
	c.mu.Lock()
	conn := c.socket
	if conn == nil {
		c.mu.Unlock()
		return
	}
	c.seq++
	body := map[string]any{"event": event, "sequenceNumber": fmt.Sprint(c.seq), "streamSid": c.stream}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = conn.Write(ctx, websocket.MessageText, b)
	cancel()
	c.mu.Unlock()
}
func (m *Mock) inject(c *call, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resource.Status != "in-progress" || c.socket == nil {
		return errors.New("No connected stream")
	}
	count := (len(data) + 159) / 160
	if cap(c.frames)-len(c.frames) < count {
		return errors.New("Inbound audio queue is full")
	}
	for i := 0; i < len(data); i += 160 {
		end := i + 160
		if end > len(data) {
			end = len(data)
		}
		c.frames <- append([]byte{}, data[i:end]...)
	}
	return nil
}
func (m *Mock) Feed(id, japanese string) error {
	c := m.get(id)
	if c == nil || len(japanese) > 4000 || strings.TrimSpace(japanese) == "" {
		return errors.New("Invalid employee input")
	}
	c.mu.Lock()
	ready := c.resource.Status == "in-progress" && c.socket != nil
	c.mu.Unlock()
	if !ready {
		return errors.New("Employee is not connected")
	}
	select {
	case c.commands <- japanese:
		return nil
	default:
		return errors.New("Employee queue is full")
	}
}
func (m *Mock) outgoing(c *call, data []byte) error {
	var b struct {
		Event, StreamSID string
		Media            struct{ Payload string }
		Mark             struct{ Name string }
	}
	if json.Unmarshal(data, &b) != nil {
		return errors.New("Invalid media JSON")
	}
	c.mu.Lock()
	if b.StreamSID != c.stream {
		c.mu.Unlock()
		return errors.New("Stream SID mismatch")
	}
	if terminal(c.resource.Status) {
		c.mu.Unlock()
		return nil
	}
	switch b.Event {
	case "media":
		audio, e := PCMU(b.Media.Payload)
		if e != nil {
			c.mu.Unlock()
			return e
		}
		c.metrics.Outbound++
		c.metrics.Bytes += len(audio)
		if c.playUntil.Before(time.Now()) {
			c.playUntil = time.Now()
		}
		c.playUntil = c.playUntil.Add(time.Duration(len(audio)) * time.Second / 8000)
		c.log("app -> mock", "media", fmt.Sprintf("%d bytes PCMU", len(audio)))
		c.mu.Unlock()
		return nil
	case "mark":
		if b.Mark.Name == "" || len(b.Mark.Name) > 200 || len(c.marks) >= 100 {
			c.mu.Unlock()
			return errors.New("Invalid mark")
		}
		if _, exists := c.marks[b.Mark.Name]; exists {
			c.mu.Unlock()
			return errors.New("Duplicate pending mark")
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.marks[b.Mark.Name] = cancel
		delay := time.Until(c.playUntil)
		name := b.Mark.Name
		c.mu.Unlock()
		go func() {
			if !wait(ctx, delay) {
				return
			}
			m.ack(c, name)
		}()
		return nil
	case "clear":
		c.metrics.Clears++
		c.playUntil = time.Now()
		names := []string{}
		for name, cancel := range c.marks {
			cancel()
			names = append(names, name)
		}
		c.log("app -> mock", "clear", "")
		c.mu.Unlock()
		for _, name := range names {
			m.ack(c, name)
		}
		return nil
	}
	c.mu.Unlock()
	return errors.New("Unsupported media event")
}
func (m *Mock) ack(c *call, name string) {
	c.mu.Lock()
	_, ok := c.marks[name]
	if ok {
		delete(c.marks, name)
		c.metrics.Marks++
		c.log("mock -> app", "mark", name)
	}
	c.mu.Unlock()
	if ok {
		m.send(c, "mark", map[string]any{"mark": map[string]string{"name": name}})
	}
}
func (m *Mock) finish(c *call, status string) {
	c.mu.Lock()
	if terminal(c.resource.Status) {
		c.mu.Unlock()
		return
	}
	c.resource.Status = status
	c.resource.End = time.Now().UTC().Format(http.TimeFormat)
	for _, cancel := range c.marks {
		cancel()
	}
	c.marks = map[string]context.CancelFunc{}
	c.mu.Unlock()
	m.send(c, "stop", map[string]any{"stop": map[string]string{"accountSid": AccountSID, "callSid": c.resource.SID}})
	c.cancel()
	m.callback(c, "completed", status)
}
func (m *Mock) Inspect(id string) map[string]any {
	if c := m.get(id); c != nil {
		return c.snapshot()
	}
	return nil
}
func (m *Mock) Close() {
	m.mu.Lock()
	m.closed = true
	calls := []*call{}
	for _, c := range m.calls {
		calls = append(calls, c)
	}
	m.mu.Unlock()
	for _, c := range calls {
		m.finish(c, "canceled")
	}
	m.cancel()
	m.wg.Wait()
}
