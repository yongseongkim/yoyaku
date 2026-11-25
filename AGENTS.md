# Working on Yoyaku

## Product requirements

Yoyaku helps a Korean-speaking customer (B) make sense of a Japanese-speaking shop employee (A) during a reservation conversation. Caption everything A says, including a request to hold or a brief acknowledgment. B decides what to say, when to speak, and whether to book. In a live session, play Japanese audio only for words B has spoken and the interpreter has committed.

The decision model decides whether the text heard so far expresses a complete thought. It returns exactly `R` for ready or `W` for wait. Keep automated replies, `wait/respond/cut` policies, generated backchannels, and automatic booking approval out of the interpreter. A translation should preserve who said what, especially when someone corrects a name or party size, adds a condition, or asks for a booking that the shop has yet to confirm. Check dates, negation, and the order of corrections as well.

Use the Korean README to run the app or try a reservation. Check the specification before changing behavior and the benchmark record before rerunning an experiment. Write project documentation and source comments in Korean. Keep this contributor guide in English and use the speaker's language for dialogue examples. [User guide](README.md) · [Specification](SPEC.md) · [Benchmark record](benchmark/README.md)

## Runtime requirements and system overview

- Build the backend, installer, model manager, CLI, and tests in Go. The launcher uses `.local-go/go/bin/go` first, then Go 1.25 or later on `PATH`. The full audio app requires macOS, Cgo, Xcode Command Line Tools, and the Kyoko Japanese voice. Keep JavaScript in the browser for capture, UI, and playback. Use Cgo to call whisper.cpp and let the native C++/Metal engines run inference. The Python scripts in the benchmark record reproduce earlier MLX experiments. [Go module](go.mod) · [toolchain pin](go-toolchain.json)
- Run `./yoyaku setup` to download and verify the pinned engines and default weights, then `./yoyaku serve`. The Gemma and Whisper weight files alone take about 14.62 GB and 0.49 GB on disk. We measured the models on a Mac with 48 GiB of unified memory. We have yet to measure the minimum memory needed to run the whole app. [Model pins](models.json) · [engine pins](native-runtimes.json)
- The rehearsal at `/` accepts microphone or typed replies. For continuous interpretation at `/live`, grant browser audio permissions and select a language for each input. Duplex capture uses a Korean microphone plus Japanese tab audio, or two distinct input devices. A single input supports one direction.

```text
Browser /      -- HTTP (script, text or recording) --+
Browser /live  -- WebSocket (audio per track) -------+--> Go app :3002
                                                           |
                                            rehearsal or live interpreter
                                                           |
                                              worker queues and HTTP
                                                  |             |
                                                  v             v
                                         speech :8092      llama-server :8091
                                          Cgo/Whisper      one GGUF child
                                                               ^
Go model manager :8093 ----------------------------------------+ start/stop/switch
Browser -- /api/speak --> Go app --> macOS say (Kyoko) -- WAV --> browser playback

Separate process: ./yoyaku phone-demo
Browser :3000 <--> scripted Go phone app <--> mock Twilio provider :3001
```

`serve` runs the app, speech server, and manager in one Go process. Workers send inference requests directly to the llama.cpp child. Keep listeners on loopback and the LLM credential server-side. Serialize model switches and check model identity before and after inference. The web app allows one live session at a time. While it is open, the user must end it before starting a rehearsal or switching models in the UI. Translation-only profiles support sentence rehearsal. Live sessions need a profile that can both judge and translate.

The phone demo can simulate calls with a phrasebook. It handles signed webhooks and TwiML, sends and receives 8 kHz PCMU audio over Media Streams, and supports playback marks and clears. The test harness delivers the employee's scripted text on a separate control channel. Connecting the audio to the interpreter, registering with a PBX over SIP, and calling public phone numbers are future work. [Phone demo specification](SPEC.md#전화-모의-환경)

## Code and shared inputs

| Location | Responsibility |
| --- | --- |
| `internal/decision` | Model input contract, R/W validation, and stream fixture loading |
| `internal/interpreter` | Stabilization, source commits, caption revisions, ordered playback, output generations, and cancellation |
| `internal/scenario` | Shared reservation scenario loader and validation for the UI, CLI, and tests |
| `internal/app`, `public` | Microphone rehearsal and continuous interpretation |
| `internal/worker` | Source-only model requests, model identity, queues, cancellation, and deadlines |
| `internal/model`, `internal/speech`, `internal/nativewhisper` | Asset installation, process ownership, and speech recognition |
| `internal/phone` | Independent Twilio mock and scripted phone app |
| `internal/cli`, `internal/research` | Entrypoints, replay, explicit experiments, and benchmark verification |
| [Reservation scenarios](testdata/reservations/scenarios.json) | Single source for 15 practice dialogues and their manual checks |
| [Decision streams](testdata/decision/streams.json) | 12 timed sequences for controller state transitions |
| [Readiness probes](testdata/decision/readiness.json) | 48 original semantic completeness probes |
| [Translation probes](testdata/translation/probes.json) | 24 original translation probes |
| [Disfluency probes](testdata/decision/disfluency.json) | 34 Korean/Japanese observations with hesitation and correction. Four premarked `?` observations are excluded from scoring. |
| [Reservation intent](testdata/translation/reservation-intent.json) | 20 translation probes with facts, conditions, and speaker roles to review |
| `testdata/audio` | Synthetic WAV inputs for integration checks |
| [Benchmark overview](benchmark/README.md) | Entry point for the model comparison, readiness cases, and translation scenarios |
| `.artifacts` | Temporary replay output, diagnostics, and new experiment results |

## Editing reservation scenarios

Edit the `scenarios` array in the shared reservation file. Use unique, stable IDs with lowercase letters, digits, and hyphens. Keep the fixture content in the language presented to the user.

```json
{
  "id": "example-headcount",
  "title": "인원 정정",
  "category": "인원·정정",
  "description": "두 명이 아니라 네 명이라는 정정을 연습합니다.",
  "checks": ["두 명과 네 명을 구분한다", "부정을 보존한다", "대신 답하지 않는다"],
  "turns": [
    {"id":"a1","speaker":"A","text":"2名様ですね。","ko":"두 분이시죠?","spoken_ja":""},
    {"id":"b1","speaker":"B","text":"","ko":"두 명이 아니라 네 명입니다.","spoken_ja":"2名ではなく4名です。"}
  ]
}
```

Give each speaker at least one turn. For A, put the Japanese line in `text` and its Korean meaning in `ko`. For B, put an example reply in `ko` and its Japanese translation in `spoken_ja`. Use `checks` for details a person can verify, such as whether a correction changes the party size or whether the shop has confirmed the booking. Leave out legacy agent fields such as `expected.action`, `policy_context`, and `requires_approval`.

Have the UI, CLI, and tests read the same scenarios through the loader or API. When adding or removing a scenario, update the catalog, API, and full-dialogue tests that check the IDs or counts. Make the fixed turn order clear in both the UI and README. B can give any reply. Translate what B actually says and show the example translation only as a reference. Keep reference translations and manual checks out of model requests.

## Decision maker and stream tests

Whisper transcribes a growing 16 kHz mono PCM16 recording and supplies timestamps for its tokens. Before asking the LLM for a decision, the Go controller must see the same candidate text twice and see it stay stable for 600 ms. It also needs either 400 ms of audio after the candidate or 700 ms of quiet. If the LLM returns `R`, check that the source and its alignment are still current, the session is open, and the deadline has not passed. Commit only the aligned portion and keep the remaining audio for the next pass. Silence alone cannot settle an unfinished condition, negation, or correction.

A can have provisional Korean captions while the model is still waiting. Revise them when the transcript changes and discard translations for older text. Play B's committed segments in order. When B stops playback, invalidate the old output generation. Hold new audio until the browser acknowledges the matching clear command.

Each `streams.json` case has `id`, `title`, `track`, `language`, and `snapshots`. Use `ja-JP` for `shop` and `ko-KR` for `user`.

| Snapshot field | Contract |
| --- | --- |
| `at_ms` | Simulated time in milliseconds. Each value must be at least as large as the previous one. |
| `text` | Entire source buffer observed at that point |
| `samples` | Window length at 16 kHz, from 4,800 to 320,000. Within a window the count can only stay constant or increase. |
| `word_end_ms` | Time at which the recognized word ends, measured from the start of the window. It must fall within that window. |
| `quiet_ms` | Observed quiet time from 0 to 5,000 ms |
| `expected` | `-` means no judge call. `R` or `W` means one call returning that label. |
| `commit` | Text the controller should commit after `R`. End the test window after a commit. |

Send the judge only the observed text, source language, and the previous utterance when needed. Keep expected labels, future observations, reference translations, and review criteria out of all inference inputs.

| Test file in `internal/interpreter` | Behavior covered |
| --- | --- |
| `decision_test.go` | Stability, incomplete clauses, corrections, prefix consumption, invalid labels and alignment |
| `decision_stream_test.go` | Replay of the shared timed cases, individually or together |
| `captions_test.go` | Source revision, provisional captions, discarding late results |
| `playback_test.go` | FIFO, output generations, stop, and matching clear acknowledgments |
| `scenarios_test.go` | Delivery of all A/B source turns |
| `session_test.go` | Input validation, lifetime, deadlines, and backpressure |

## Verification and manual checks

```sh
make test-decision
make test-scenarios
make test
make vet
./yoyaku verify
```

Run the most relevant tests first. Include the race detector when changing state, queues, cancellation, or concurrency. Offline stream tests use fake ASR and LLM outputs to exercise timing and state transitions. For documentation changes, check the links and fixture coverage without loading models.

```sh
./yoyaku scenarios show party-mismatch
./yoyaku serve
# Leave the service running and replay a case from a second terminal.
./yoyaku decision run ja-greeting
./yoyaku decision run ko-condition-pause
```

`decision run CASE` replays virtual ASR observations through the actual Go controller and active LLM. Its clock is simulated and the run bypasses microphone capture, STT, translation, and TTS. Use `./yoyaku check-live` with the app running and the live session free for a synthetic-audio integration check.

On the rehearsal page, inspect the ASR source first, compare the translation, then record manual checks. The user explicitly downloads the record. Use generated audio or a virtual MediaStream in browser automation. State whether a result used a fake or real model, synthetic or live speech, and manual or automated evaluation. Report delivery failures separately from linguistic errors.

## Preserving local LLM benchmarks

Start with the short overview. In `benchmark/models.md`, record which models ran on which runtime and input set, how many replies were graded, and the results that affected the choice. Group the tested phrases by conversational situation in `benchmark/readiness.md` and `benchmark/translation.md`. Link to the reusable inputs under `testdata`. [Benchmark overview](benchmark/README.md)

Keep raw requests, outputs, and diagnostic files under `.artifacts`. Benchmark pages should be readable as plain Markdown without an embedded archive. The previous six runs have a local ZIP and SHA-256 checksum under `.artifacts/benchmark-evidence/`. The app and its tests work without them. The `verify` command checks local file links in the benchmark pages and also checks the original reports and judgments when that ZIP is available.

Grade each new translation against its source and record why. A successful request tells us a result was delivered. A string or regex match checks only the wording. Keep the original labels and scores even when a development prompt is ambiguous. When reporting latency, say whether the run called MLX directly or used the app's cached GGUF HTTP endpoint.

Run `./yoyaku benchmark` or `./yoyaku benchmark disfluency` explicitly for the affected models in a new output directory when the model, quantization, prompt, template, decoding, runtime, inputs, or measurement objective changes. App startup, ordinary tests, and model switching must leave benchmark execution opt-in. Keep reusable input in `testdata` and temporary output in `.artifacts`. The benchmark record covers local LLM experiments. Leave browser checks, synthetic-audio tests, Go migration logs, and copies of the application code out of it. Keep the installed weights in `.local-native/models` when cleaning up experiment files.
