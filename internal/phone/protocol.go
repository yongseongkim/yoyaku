// phone 패키지는 Twilio Voice의 통화 절차를 로컬에서 재현한다. 실제 전화망에 연결하지 않는다.
package phone

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const AccountSID = "AC00000000000000000000000000000000"
const AuthToken = "local-mock-token"
const APIVersion = "2010-04-01"

func sid(prefix string) string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b)
}
func terminal(status string) bool {
	switch status {
	case "completed", "busy", "no-answer", "failed", "canceled":
		return true
	}
	return false
}
func Sign(rawURL string, values url.Values, token string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(rawURL)
	for _, key := range keys {
		v := append([]string(nil), values[key]...)
		sort.Strings(v)
		last := ""
		for i, value := range v {
			if i > 0 && value == last {
				continue
			}
			b.WriteString(key)
			b.WriteString(value)
			last = value
		}
	}
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
func Verify(rawURL string, v url.Values, signature, token string) bool {
	return subtle.ConstantTimeCompare([]byte(Sign(rawURL, v, token)), []byte(signature)) == 1
}
func Loopback(raw string, websocket bool) error {
	u, e := url.Parse(raw)
	if e != nil {
		return e
	}
	allowed := u.Scheme == "http" || u.Scheme == "https"
	if websocket {
		allowed = u.Scheme == "ws" || u.Scheme == "wss"
	}
	if !allowed || u.User != nil || u.Fragment != "" || u.Host == "" {
		return errors.New("Use a local URL")
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return nil
	}
	return errors.New("Only loopback endpoints are supported")
}
func PCMU(payload string) ([]byte, error) {
	data, e := base64.StdEncoding.Strict().DecodeString(payload)
	if e != nil || len(data) == 0 || len(data) > 48000 || base64.StdEncoding.EncodeToString(data) != payload || bytes.HasPrefix(data, []byte("RIFF")) || bytes.HasPrefix(data, []byte("OggS")) {
		return nil, errors.New("Use raw base64 PCMU without an audio header")
	}
	return data, nil
}
func Silence() string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, 160)) }
func reply(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, code int, message string) {
	providerCode := 20001
	if code == 401 {
		providerCode = 20003
	}
	if code == 404 {
		providerCode = 20404
	}
	if message == "Invalid To number" {
		providerCode = 21211
	}
	if message == "Invalid From number" {
		providerCode = 21212
	}
	reply(w, code, map[string]any{"error": message, "message": message, "status": code, "code": providerCode, "more_info": "local-mock"})
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	if e := d.Decode(value); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("Trailing JSON")
	}
	return nil
}
func form(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if e != nil {
		return nil, e
	}
	return url.ParseQuery(string(b))
}

var httpClient = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Redirects are disabled") }}

func post(ctx context.Context, rawURL string, values url.Values, token string) ([]byte, error) {
	if e := Loopback(rawURL, false); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(values.Encode()))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", Sign(rawURL, values, token))
	response, e := httpClient.Do(req)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Webhook HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 65536))
}
