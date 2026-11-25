package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"yoyaku/internal/pcm"
)

func checkLive(parent context.Context, root string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(parent, 100*time.Second)
	defer cancel()
	base := "http://127.0.0.1:3002"
	conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1:3002/api/live", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {base}}})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 * 1024 * 1024)
	events := []map[string]any{}
	write := func(v any) error { b, _ := json.Marshal(v); return conn.Write(ctx, websocket.MessageText, b) }
	read := func() (map[string]any, error) {
		_, b, e := conn.Read(ctx)
		if e != nil {
			return nil, e
		}
		var event map[string]any
		if e = json.Unmarshal(b, &event); e != nil {
			return nil, e
		}
		events = append(events, event)
		if event["type"] == "error" || event["type"] == "recovery" {
			return event, fmt.Errorf("Live recovery/error: %v", event)
		}
		return event, nil
	}
	if event, e := read(); e != nil || event["type"] != "ready" {
		return errors.New("Live socket did not become ready")
	}
	spoken := 0
	captions := 0
	audioBytes := 0
	for _, track := range []string{"user", "shop"} {
		file := "user.wav"
		if track == "shop" {
			file = "shop.wav"
		}
		data, e := os.ReadFile(filepath.Join(root, "testdata/audio", file))
		if e != nil {
			return e
		}
		wav, e := pcm.Normalize(data)
		if e != nil {
			return e
		}
		remaining := wav[44:]
		for window := 1; len(remaining) >= 9600 && window <= 8; window++ {
			id := fmt.Sprintf("%s-%d", track, window)
			header := append([]byte{}, wav[:44]...)
			windowWAV := append(header, remaining...)
			if window > 1 {
				// 앞부분을 덜어낸 뒤에는 남은 샘플 수에 맞게 WAV 헤더의 길이도 바꿔야 한다.
				windowWAV = wavFromPCM(remaining)
			}
			send := func(sequence int) error {
				return write(map[string]any{"type": "audio", "track": track, "window_id": id, "sequence": sequence, "samples": len(remaining) / 2, "quiet_ms": 900, "wav": base64.StdEncoding.EncodeToString(windowWAV)})
			}
			if e = send(1); e != nil {
				return e
			}
			for {
				ev, e := read()
				if e != nil {
					return e
				}
				if ev["type"] == "audio.processed" && ev["window_id"] == id {
					break
				}
			}
			timer := time.NewTimer(650 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			if e = send(2); e != nil {
				return e
			}
			consumed := 0
			translated := false
			for consumed == 0 || !translated {
				ev, e := read()
				if e != nil {
					return e
				}
				switch ev["type"] {
				case "audio.consume":
					if ev["window_id"] == id {
						consumed = int(ev["through_samples"].(float64)) * 2
					}
				case "caption.update":
					if ev["segment_id"] == id && ev["provisional"] == false {
						captions++
						translated = true
					}
				case "tts.request":
					if ev["segment_id"] != id {
						continue
					}
					translated = true
					spoken++
					b, _ := json.Marshal(map[string]any{"text": ev["text"]})
					req, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/speak", bytes.NewReader(b))
					req.Header.Set("Content-Type", "application/json")
					response, e := http.DefaultClient.Do(req)
					if e != nil {
						return e
					}
					audio, e := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
					response.Body.Close()
					if e != nil || response.StatusCode != 200 || len(audio) < 44 {
						return errors.New("Japanese speech generation failed")
					}
					audioBytes += len(audio)
				}
			}
			if consumed < 1 || consumed > len(remaining) {
				return errors.New("Invalid source consumption")
			}
			remaining = remaining[consumed:]
		}
	}
	if err = write(map[string]string{"type": "user.stop_output", "command_id": "native-check-stop"}); err != nil {
		return err
	}
	for {
		ev, e := read()
		if e != nil {
			return e
		}
		if ev["type"] == "playback.clear" {
			if err = write(map[string]string{"type": "playback.cleared", "command_id": "native-check-stop"}); err != nil {
				return err
			}
			break
		}
	}
	report := map[string]any{"protocol": "go-native-live-smoke-v1", "synthetic_audio": true, "spoken_segments": spoken, "final_captions": captions, "tts_wav_bytes": audioBytes, "events": events, "quality": "Integration evidence; real microphone and telephone audio accuracy remain unmeasured."}
	data, _ := json.MarshalIndent(report, "", "  ")
	dir := filepath.Join(root, ".artifacts/native-live")
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Native live check: %d spoken segments, %d final captions, %d bytes Japanese WAV. Report: %s\n", spoken, captions, audioBytes, dir)
	if spoken == 0 || captions == 0 {
		return errors.New("Missing interpretation outputs")
	}
	return nil
}
func wavFromPCM(data []byte) []byte {
	samples := make([]float32, len(data)/2)
	for i := range samples {
		samples[i] = float32(int16(uint16(data[i*2])|uint16(data[i*2+1])<<8)) / 32768
	}
	return pcm.Encode(samples)
}
