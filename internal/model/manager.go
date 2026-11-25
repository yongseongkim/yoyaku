package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Process는 모델 관리자가 직접 띄운 자식 프로세스다.
// 다음 모델이 같은 포트를 쓰므로 Stop은 이전 프로세스가 종료된 뒤 반환해야 한다.
type Process interface {
	Ready(context.Context) error
	Stop()
	Done() <-chan struct{}
}
type Status struct {
	State   string `json:"state"`
	Active  string `json:"active"`
	Target  string `json:"target"`
	Default string `json:"default"`
	Error   string `json:"error,omitempty"`
}
type Manager struct {
	mu                sync.Mutex
	root              string
	catalog           Catalog
	state             Status
	process           Process
	switching, closed bool
	ctx               context.Context
	cancel            context.CancelFunc
	operations        sync.WaitGroup
	Launch            func(string) (Process, error)
	Installed         func(string) bool
	Save              func(string) error
}

func NewManager(ctx context.Context, root string, c Catalog) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{root: root, catalog: c, ctx: ctx, cancel: cancel, state: Status{State: "stopped", Default: c.Default}}
	m.Installed = func(id string) bool { return c.Profiles[id].Installed(root, id) }
	m.Launch = func(id string) (Process, error) { return launch(root, id, c.Profiles[id]) }
	m.Save = func(id string) error {
		b, _ := json.Marshal(map[string]string{"profile": id})
		return atomicWrite(filepath.Join(root, ".local-native/selected-model.json"), b)
	}
	return m
}
func (m *Manager) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.state }
func (m *Manager) Selected() (string, error) {
	b, e := os.ReadFile(filepath.Join(m.root, ".local-native/selected-model.json"))
	if os.IsNotExist(e) {
		return m.catalog.Default, nil
	}
	if e != nil {
		return "", e
	}
	var s struct {
		Profile string `json:"profile"`
	}
	if json.Unmarshal(b, &s) != nil {
		return "", errors.New("Invalid saved model selection")
	}
	if _, ok := m.catalog.Profiles[s.Profile]; !ok {
		return "", errors.New("Unknown saved model")
	}
	return s.Profile, nil
}
func (m *Manager) Switch(id string) (err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("Model manager is closed")
	}
	if _, ok := m.catalog.Profiles[id]; !ok {
		m.mu.Unlock()
		return errors.New("Unknown model")
	}
	if m.switching {
		m.mu.Unlock()
		return errors.New("Another switch is in progress")
	}
	if !m.Installed(id) {
		m.mu.Unlock()
		return fmt.Errorf("Model %s is not installed; run ./yoyaku models setup %s", id, id)
	}
	if m.state.State == "ready" && m.state.Active == id {
		m.mu.Unlock()
		return nil
	}
	m.switching = true
	m.operations.Add(1)
	m.state = Status{State: "stopping", Target: id, Default: m.catalog.Default}
	old := m.process
	m.mu.Unlock()
	defer m.operations.Done()
	defer func() {
		m.mu.Lock()
		m.switching = false
		if err != nil {
			m.state = Status{State: "error", Default: m.catalog.Default, Error: err.Error()}
		}
		m.mu.Unlock()
	}()
	if old != nil {
		old.Stop()
	}
	if err = m.ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.process = nil
	m.state.State = "loading"
	m.mu.Unlock()
	p, e := m.Launch(id)
	if e != nil {
		return e
	}
	m.mu.Lock()
	m.process = p
	m.mu.Unlock()
	ready, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
	defer cancel()
	if err = p.Ready(ready); err != nil {
		p.Stop()
		return err
	}
	if err = m.ctx.Err(); err != nil {
		p.Stop()
		return err
	}
	if err = m.Save(id); err != nil {
		p.Stop()
		return err
	}
	m.mu.Lock()
	select {
	case <-p.Done():
		m.mu.Unlock()
		return errors.New("Model exited during startup")
	default:
	}
	m.state = Status{State: "ready", Active: id, Default: m.catalog.Default}
	m.mu.Unlock()
	go func() {
		<-p.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.process == p && m.state.State == "ready" {
			m.state = Status{State: "error", Default: m.catalog.Default, Error: "Native model exited; select a model to retry"}
		}
	}()
	return nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.operations.Wait()
	m.mu.Lock()
	p := m.process
	m.process = nil
	m.state = Status{State: "stopped", Default: m.catalog.Default}
	m.mu.Unlock()
	if p != nil {
		p.Stop()
	}
}
func (m *Manager) Models() map[string]any {
	s := m.Status()
	models := []map[string]any{}
	ids := []string{}
	for id := range m.catalog.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := m.catalog.Profiles[id]
		models = append(models, map[string]any{"id": id, "name": p.Name, "installed": m.Installed(id), "translation_only": p.TranslationOnly, "default": id == m.catalog.Default})
	}
	return map[string]any{"state": s.State, "active": s.Active, "target": s.Target, "default": s.Default, "error": s.Error, "models": models, "engine": "go+llama.cpp"}
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" && r.URL.Path == "/status" {
		json.NewEncoder(w).Encode(m.Status())
		return
	}
	if r.Method == "GET" && r.URL.Path == "/models" {
		json.NewEncoder(w).Encode(m.Models())
		return
	}
	if r.Method != "POST" || r.URL.Path != "/switch" {
		http.Error(w, "Not found", 404)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "Use the app proxy", 403)
		return
	}
	var b struct {
		Profile string `json:"profile"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&b) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	if err := m.Switch(b.Profile); err != nil {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(m.Status())
}

type nativeProcess struct {
	cmd   *exec.Cmd
	done  chan struct{}
	once  sync.Once
	alias string
	key   string
}

func launch(root, id string, p Profile) (Process, error) {
	key, e := LocalKey(root)
	if e != nil {
		return nil, e
	}
	probe, e := net.Listen("tcp", "127.0.0.1:8091")
	if e != nil {
		return nil, errors.New("Port 8091 is occupied; stop its owner before starting a model")
	}
	probe.Close()
	logdir := filepath.Join(root, ".local-native/logs")
	if e = os.MkdirAll(logdir, 0755); e != nil {
		return nil, e
	}
	logfile, e := os.OpenFile(filepath.Join(logdir, "llama.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return nil, e
	}
	alias := "/models/" + p.Directory
	cmd := exec.Command(filepath.Join(root, ".local-native/llama/llama-server"), "--model", p.Path(root, id), "--alias", alias, "--host", "127.0.0.1", "--port", "8091", "--ctx-size", "8192", "--parallel", "1", "--cache-ram", "128", "--reasoning-budget", "0", "--no-webui", "--fit", "on", "--fit-target", "8192", "--api-key-file", filepath.Join(root, ".local-native/worker.key"), "--cors-origins", "http://127.0.0.1:3002")
	cmd.Dir = root
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	if e = cmd.Start(); e != nil {
		logfile.Close()
		return nil, e
	}
	child := &nativeProcess{cmd: cmd, done: make(chan struct{}), alias: alias, key: key}
	go func() { _ = cmd.Wait(); logfile.Close(); close(child.done) }()
	return child, nil
}
func (p *nativeProcess) Done() <-chan struct{} { return p.done }
func (p *nativeProcess) Stop() {
	p.once.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(8 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}
func (p *nativeProcess) Ready(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return errors.New("llama-server exited; see .local-native/logs/llama.log")
		case <-ticker.C:
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8091/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+p.key)
			resp, e := client.Do(req)
			if e != nil {
				continue
			}
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			e = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&body)
			resp.Body.Close()
			if e == nil && resp.StatusCode == 200 && len(body.Data) == 1 && body.Data[0].ID == p.alias {
				return nil
			}
		}
	}
}
