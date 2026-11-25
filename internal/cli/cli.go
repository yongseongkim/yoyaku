package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
	"yoyaku/internal/app"
	"yoyaku/internal/model"
	"yoyaku/internal/nativewhisper"
	"yoyaku/internal/phone"
	"yoyaku/internal/speech"
	"yoyaku/internal/worker"
)

func Run(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("yoyaku", flag.ContinueOnError)
	f.SetOutput(out)
	root := f.String("root", ".", "프로젝트 디렉터리")
	addr := f.String("addr", "127.0.0.1:3002", "웹 서버가 요청을 받을 주소")
	if err := f.Parse(args); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	*root = absolute
	args = f.Args()
	if len(args) == 0 {
		args = []string{"serve"}
	}
	catalog, err := model.Read(*root)
	if err != nil {
		return err
	}
	printJSON := func(v any) error { enc := json.NewEncoder(out); enc.SetIndent("", "  "); return enc.Encode(v) }
	switch args[0] {
	case "help":
		fmt.Fprintln(out, `사용법  ./yoyaku [--root PATH] [--addr 127.0.0.1:3002] 명령

설치와 실행
  setup [MODEL]                  추론 엔진과 Whisper, 선택한 LLM을 설치합니다. 모델을 생략하면 기본 모델을 씁니다.
  serve                          통역 앱과 음성 서버, 모델 관리자를 함께 실행합니다.
  app                            웹 앱만 실행합니다.
  speech                         음성 인식 서버만 실행합니다.
  speech setup                   음성 인식에 필요한 파일만 설치합니다.
  models list                    선택할 수 있는 모델 목록을 봅니다.
  models status                  실행 중인 모델의 상태를 봅니다.
  models setup ID                지정한 모델을 설치합니다.
  models switch ID               실행 중인 LLM을 지정한 모델로 바꿉니다.
  models serve [ID]              모델 관리자만 실행합니다.

예약 연습과 모의 통화
  scenarios list|show CASE|json  예약 상황 목록이나 특정 대화를 봅니다. json은 전체 대화를 출력합니다.
  decision list|run CASE         판단 사례를 보거나, 가상 인식 결과를 실제 LLM에 보내 R/W 응답을 확인합니다.
  phone-demo                     모의 전화 앱과 Twilio 모의 서버를 함께 실행합니다.
  mock                           Twilio 모의 서버만 실행합니다.

점검과 실험
  verify                         벤치마크 문서의 파일 링크를 확인하고, 로컬 원본이 있으면 함께 검증합니다.
  check                          실행 중인 LLM으로 번역 시점 판단과 번역을 점검합니다.
  check-audio WAV ko|ja          WAV 파일을 한국어 또는 일본어로 인식한 결과를 봅니다.
  check-live                     합성 음성으로 양방향 통역 연결을 점검합니다.
  benchmark [disfluency]         기본 문구로 모델을 평가합니다. disfluency는 머뭇거림과 예약 의도를 시험합니다.`)
		return nil
	case "setup":
		id := catalog.Default
		if len(args) > 1 {
			id = args[1]
		}
		p, ok := catalog.Profiles[id]
		if !ok {
			return errors.New("Unknown model")
		}
		if err = model.InstallRuntimes(ctx, *root, out); err != nil {
			return err
		}
		if err = model.InstallModel(ctx, *root, "whisper-small", catalog.Speech, out); err != nil {
			return err
		}
		return model.InstallModel(ctx, *root, id, p, out)
	case "models":
		if len(args) < 2 {
			return errors.New("Use models list|setup|switch|status|serve")
		}
		switch args[1] {
		case "setup":
			if len(args) != 3 {
				return errors.New("Use models setup MODEL")
			}
			p, ok := catalog.Profiles[args[2]]
			if !ok {
				return errors.New("Unknown model")
			}
			return model.InstallModel(ctx, *root, args[2], p, out)
		case "list":
			m := model.NewManager(ctx, *root, catalog)
			defer m.Close()
			return printJSON(m.Models())
		case "serve":
			id := ""
			if len(args) > 2 {
				id = args[2]
			}
			return serve(ctx, *root, *addr, catalog, false, true, false, id, out)
		case "status", "switch":
			path := "/models"
			method := "GET"
			var body io.Reader
			if args[1] == "switch" {
				if len(args) != 3 {
					return errors.New("Use models switch MODEL")
				}
				b, _ := json.Marshal(map[string]string{"profile": args[2]})
				body = bytes.NewReader(b)
				path = "/switch"
				method = "POST"
			}
			req, _ := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:8093"+path, body)
			req.Header.Set("Content-Type", "application/json")
			response, e := (&http.Client{Timeout: 190 * time.Second}).Do(req)
			if e != nil {
				return e
			}
			defer response.Body.Close()
			_, e = io.Copy(out, io.LimitReader(response.Body, 1048576))
			if e != nil {
				return e
			}
			if response.StatusCode != 200 {
				return fmt.Errorf("Manager HTTP %d", response.StatusCode)
			}
			return nil
		}
		return errors.New("Unknown models command")
	case "serve":
		return serve(ctx, *root, *addr, catalog, true, true, true, "", out)
	case "app":
		return serve(ctx, *root, *addr, catalog, true, false, false, "", out)
	case "speech":
		if len(args) > 1 && args[1] == "setup" {
			if err := model.InstallRuntimes(ctx, *root, out); err != nil {
				return err
			}
			return model.InstallModel(ctx, *root, "whisper-small", catalog.Speech, out)
		}
		return serve(ctx, *root, *addr, catalog, false, false, true, "", out)
	case "mock":
		return servePhone(ctx, *root, false, out)
	case "phone-demo":
		return servePhone(ctx, *root, true, out)
	case "verify":
		return verify(*root, out)
	case "check":
		return check(ctx, *root, out)
	case "check-audio":
		return checkAudio(ctx, *root, args[1:], out)
	case "check-live":
		return checkLive(ctx, *root, out)
	case "benchmark":
		return benchmark(ctx, *root, args[1:], out)
	case "scenarios":
		return scenarios(*root, args[1:], out)
	case "decision":
		return decisionCommand(ctx, *root, args[1:], out)
	default:
		return fmt.Errorf("Unknown command %s; run ./yoyaku help", args[0])
	}
}
func servePhone(parent context.Context, root string, ui bool, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	mock := phone.NewMock(ctx)
	defer mock.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:3001")
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: mock, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	defer server.Close()
	results := make(chan error, 2)
	go func() { results <- server.Serve(listener) }()
	fmt.Fprintln(out, "Go Twilio mock: http://127.0.0.1:3001")
	if ui {
		browser, err := net.Listen("tcp", "127.0.0.1:3000")
		if err != nil {
			return err
		}
		defer browser.Close()
		app := phone.NewApplication(ctx, root, "http://127.0.0.1:3000", "http://127.0.0.1:3001", mock)
		defer app.Close()
		web := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
		defer web.Close()
		go func() { results <- web.Serve(browser) }()
		fmt.Fprintln(out, "Scripted phone demo: http://127.0.0.1:3000")
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-results:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
func serve(parent context.Context, root, addr string, catalog model.Catalog, web, llm, stt bool, selection string, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type endpoint struct {
		listener net.Listener
		server   *http.Server
	}
	endpoints := []endpoint{}
	errorsCh := make(chan error, 3)
	defer func() {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		for _, e := range endpoints {
			_ = e.server.Shutdown(shutdown)
			_ = e.listener.Close()
		}
	}()
	bind := func(address string) (net.Listener, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return nil, errors.New("Use numeric loopback listeners")
		}
		listener, e := net.Listen("tcp", address)
		if e == nil {
			endpoints = append(endpoints, endpoint{listener: listener, server: &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, BaseContext: func(net.Listener) context.Context { return ctx }}})
		}
		return listener, e
	}
	indexes := map[string]int{}
	for _, item := range []struct {
		enabled       bool
		name, address string
	}{{llm, "manager", "127.0.0.1:8093"}, {stt, "speech", "127.0.0.1:8092"}, {web, "web", addr}} {
		if item.enabled {
			if _, err := bind(item.address); err != nil {
				return err
			}
			indexes[item.name] = len(endpoints) - 1
		}
	}
	start := func(name string, handler http.Handler) {
		e := endpoints[indexes[name]]
		e.server.Handler = handler
		go func() {
			err := e.server.Serve(e.listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case errorsCh <- err:
				default:
				}
			}
		}()
	}
	if llm {
		manager := model.NewManager(ctx, root, catalog)
		defer manager.Close()
		start("manager", manager)
		if selection == "" {
			var err error
			selection, err = manager.Selected()
			if err != nil {
				return err
			}
		}
		fmt.Fprintln(out, "Loading native LLM:", selection)
		if err := manager.Switch(selection); err != nil {
			return err
		}
		fmt.Fprintln(out, "LLM ready: 127.0.0.1:8091 (Go manager: 8093)")
	}
	if stt {
		if !catalog.Speech.Installed(root, "whisper-small") {
			return errors.New("Whisper is not installed; run ./yoyaku setup")
		}
		recognizer, err := nativewhisper.New(root, catalog.Speech.Path(root, "whisper-small"))
		if err != nil {
			return err
		}
		defer func() { cancel(); recognizer.Close() }()
		start("speech", speech.New(recognizer))
		fmt.Fprintln(out, "Native speech ready: 127.0.0.1:8092")
	}
	if web {
		client, err := worker.New(ctx, root)
		if err != nil {
			return err
		}
		handler, err := app.New(ctx, root, client)
		if err != nil {
			return err
		}
		defer handler.Close()
		start("web", handler)
		fmt.Fprintf(out, "Go interpreter: http://%s/live (rehearsal: /)\n", addr)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsCh:
		return err
	}
}
