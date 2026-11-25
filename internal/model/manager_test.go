package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	done  chan struct{}
	ready chan struct{}
	once  sync.Once
}

func (p *fakeProcess) Ready(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("Exited")
	case <-p.ready:
		return nil
	}
}
func (p *fakeProcess) Stop()                 { p.once.Do(func() { close(p.done) }) }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func fakeManager(t *testing.T) (*Manager, *[]*fakeProcess) {
	t.Helper()
	m := NewManager(context.Background(), t.TempDir(), Catalog{Default: "a", Profiles: map[string]Profile{"a": {}, "b": {}, "missing": {}}})
	m.Installed = func(id string) bool { return id != "missing" }
	m.Save = func(string) error { return nil }
	processes := []*fakeProcess{}
	m.Launch = func(string) (Process, error) {
		if len(processes) > 0 {
			select {
			case <-processes[len(processes)-1].done:
			default:
				t.Error("Second worker started before first exited")
			}
		}
		p := &fakeProcess{done: make(chan struct{}), ready: make(chan struct{})}
		close(p.ready)
		processes = append(processes, p)
		return p, nil
	}
	t.Cleanup(m.Close)
	return m, &processes
}
func TestSwitchOwnsOneWorker(t *testing.T) {
	m, p := fakeManager(t)
	if e := m.Switch("a"); e != nil {
		t.Fatal(e)
	}
	if e := m.Switch("a"); e != nil || len(*p) != 1 {
		t.Fatal("Ready same model should be a no-op")
	}
	for _, id := range []string{"missing", "unknown"} {
		if e := m.Switch(id); e == nil {
			t.Fatal("Invalid switch succeeded")
		}
		if m.Status().Active != "a" {
			t.Fatal("Invalid target stopped active model")
		}
	}
	if e := m.Switch("b"); e != nil {
		t.Fatal(e)
	}
	if len(*p) != 2 || m.Status().Active != "b" {
		t.Fatal(m.Status())
	}
	(*p)[1].Stop()
	deadline := time.Now().Add(time.Second)
	for m.Status().State != "error" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Status().State != "error" {
		t.Fatal("Unexpected model exit was not reported")
	}
}
func TestCloseCancelsLoadingAndRejectsConcurrentSwitch(t *testing.T) {
	m, _ := fakeManager(t)
	entered := make(chan struct{})
	p := &fakeProcess{done: make(chan struct{}), ready: make(chan struct{})}
	m.Launch = func(string) (Process, error) { close(entered); return p, nil }
	done := make(chan error, 1)
	go func() { done <- m.Switch("a") }()
	<-entered
	if m.Switch("b") == nil {
		t.Fatal("Concurrent switch accepted")
	}
	m.Close()
	if <-done == nil {
		t.Fatal("Canceled load succeeded")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("Owned child was not stopped")
	}
	if m.Switch("a") == nil {
		t.Fatal("Closed manager accepted a switch")
	}
}
func TestDownloadResumeVerifiesBeforePublish(t *testing.T) {
	data := []byte("abcdef-pinned-artifact")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	dir := t.TempDir()
	path := filepath.Join(dir, "model.bin")
	_ = os.WriteFile(path+".part", data[:5], 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=5-" {
			t.Error("Resume offset missing")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(206)
		w.Write(data[5:])
	}))
	defer server.Close()
	if e := Download(context.Background(), server.URL, path, digest, int64(len(data)), io.Discard); e != nil {
		t.Fatal(e)
	}
	actual, _ := os.ReadFile(path)
	if string(actual) != string(data) {
		t.Fatal("Wrong downloaded file")
	}
	if e := Download(context.Background(), server.URL, path, digest, int64(len(data)), io.Discard); e != nil {
		t.Fatal(e)
	}
}
func TestDownloadRejectsWrongHashAndRange(t *testing.T) {
	for _, badRange := range []bool{false, true} {
		t.Run(fmt.Sprint(badRange), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model")
			if badRange {
				os.WriteFile(path+".part", []byte("x"), 0600)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if badRange {
					w.Header().Set("Content-Range", "bytes 0-2/3")
					w.WriteHeader(206)
				}
				w.Write([]byte("abc"))
			}))
			defer server.Close()
			if Download(context.Background(), server.URL, path, "invalid", 3, io.Discard) == nil {
				t.Fatal("Unverified artifact accepted")
			}
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("Invalid file was published")
			}
		})
	}
}
func TestPinnedCatalog(t *testing.T) {
	c, e := Read("../..")
	if e != nil {
		t.Fatal(e)
	}
	if c.Default != "gemma26b" {
		t.Fatal("Unexpected default profile")
	}
	for _, id := range []string{"2b", "4b", "qwen9b", "qwen27b", "gemma12b", "gemma26b", "translate12b", "kimivl", "kimi48b", "lfm8b", "hymt18b"} {
		if _, ok := c.Profiles[id]; !ok {
			t.Fatalf("Missing switchable profile %s", id)
		}
	}
	for _, p := range c.Profiles {
		p.File = "../escape"
		if p.Validate() == nil {
			t.Fatal("Unsafe model file accepted")
		}
	}
	if _, e := safePath(t.TempDir(), "../../escape"); e == nil {
		t.Fatal("Unsafe archive path accepted")
	}
}
