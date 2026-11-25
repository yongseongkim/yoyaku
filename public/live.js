import { captureContinuous, audioBase64 } from './continuous-audio.js';
import { SpeechQueue } from './speech-queue.js';

const $ = id => document.getElementById(id);
let epoch = 0, socket = null, captures = {}, streams = [], output = null, speech = null;
let running = false, starting = false, services = {}, stopId = 0, trace = [], rows = new Map();
const error = text => { $('live-error').textContent = text; $('live-error').hidden = !text; };
function refresh() {
  const mode = $('live-mode').value, active = running || starting;
  $('live-start').disabled = active || !services.live;
  $('live-end').disabled = !active; $('live-stop-output').disabled = !running;
  $('live-mode').disabled = active; $('user-device').disabled = active || mode === 'shop';
  $('shop-device').disabled = active || !['shop', 'duplex-mics'].includes(mode);
  $('live-check').disabled = active;
}
async function devices() {
  if (!navigator.mediaDevices?.enumerateDevices) return;
  const inputs = (await navigator.mediaDevices.enumerateDevices()).filter(device => device.kind === 'audioinput');
  for (const id of ['user-device', 'shop-device']) {
    const selected = $(id).value;
    $(id).replaceChildren(...[{ deviceId: '', label: '기본 마이크' }, ...inputs.filter(device => device.deviceId !== 'default')].map((device, i) => {
      const option = document.createElement('option'); option.value = device.deviceId; option.textContent = device.label || `입력 장치 ${i}`; return option;
    }));
    if ([...$(id).options].some(option => option.value === selected)) $(id).value = selected;
  }
}
async function health() {
  try {
    const response = await fetch('/api/health'); services = await response.json();
    $('live-health').textContent = `모델 ${services.model ?? '꺼짐'} · 한국어/일본어 STT ${services.languages?.includes('ja') ? '연결됨' : '업데이트/실행 필요'}`;
    if (!services.live) error('연속 통역에는 번역 시점을 판단하는 범용 모델과 한국어·일본어 음성 인식이 필요합니다.');
    else error('');
  } catch { error('로컬 시연 서버에 연결할 수 없습니다.'); }
  refresh();
}
function log(event) {
  if (['audio.processed', 'audio.consume'].includes(event.type)) return;
  const { type, track, text, label, reason, message, latency_ms, prompt_version, stt_prompt_version, at_ms, model } = event;
  trace.push(JSON.stringify({ at_ms, type, track, text, label, reason, message, latency_ms, prompt_version, stt_prompt_version, model }));
  if (trace.length > 60) trace.shift(); $('live-trace').textContent = trace.join('\n');
}
function row(track, id) {
  const key = `${track}:${id}`; if (rows.has(key)) return rows.get(key);
  $('live-conversation').querySelector('.empty')?.remove();
  const article = document.createElement('article'); article.className = `message ${track}`;
  for (const [name, text] of [['role', track === 'shop' ? 'A · 日本語 → 한국어' : 'B · 한국어 → 日本語'], ['original', ''], ['translated pending', '인식과 번역을 기다리고 있어요…'], ['meta', '잠정 원문']]) {
    const p = document.createElement('p'); p.className = name; p.textContent = text;
    if (name === 'original') p.lang = track === 'shop' ? 'ja' : 'ko';
    if (name.startsWith('translated')) p.lang = track === 'shop' ? 'ko' : 'ja';
    article.append(p);
  }
  $('live-conversation').append(article); rows.set(key, article);
  if (rows.size > 80) { const old = rows.keys().next().value; rows.get(old).remove(); rows.delete(old); }
  return article;
}
function send(message) { if (socket?.readyState === 1) socket.send(JSON.stringify(message)); }
function render(event) {
  log(event);
  if (event.type === 'audio.processed') return captures[event.track]?.buffer.ack(event.window_id, event.sequence, event.reason);
  if (event.type === 'audio.consume') return captures[event.track]?.buffer.consume(event.window_id, event.through_samples);
  if (event.type === 'audio.discard') return captures[event.track]?.buffer.discard(event.window_id);
  if (event.type === 'playback.clear') { speech?.clear(event.next_generation); send({ type: 'playback.cleared', command_id: event.command_id }); return; }
  if (event.type === 'recovery') { error(event.message ?? '의미를 더 확인하고 있어요. 이어서 말하거나 통역을 다시 시작해 주세요.'); return; }
  if (event.type === 'error') return end(event.message);
  if (event.type === 'chunk.wait') { $('live-status').textContent = event.reason === 'meaning_incomplete' ? '이어지는 말을 기다려요' : '원문 안정화 확인 중'; return; }
  if (event.type === 'chunk.decision') { $('live-status').textContent = `${event.track === 'shop' ? '일본어' : '한국어'} 구간 ${event.label === 'R' ? '의미 완결' : '이어서 듣기'}`; return; }
  if (event.type === 'transcript' || event.type === 'source.commit') {
    const item = row(event.track, event.window_id); item.querySelector('.original').textContent = event.text;
    item.querySelector('.meta').textContent = event.type === 'source.commit' ? '안정된 의미 구간 확정' : `인식 ${event.latency_ms ?? '—'}ms · 잠정 원문`;
  } else if (event.type === 'caption.update') {
    const item = row('shop', event.segment_id); item.querySelector('.translated').textContent = event.text;
    item.querySelector('.translated').classList.toggle('pending', event.provisional);
    item.querySelector('.meta').textContent = `${event.provisional ? '잠정 자막 · 이후 수정될 수 있어요' : '확정 자막'} · ${event.model ?? ''}`;
  } else if (event.type === 'caption.invalidate') {
    const item = row('shop', event.segment_id); item.querySelector('.translated').textContent = '원문 변경 · 자막 수정 중…';
    item.querySelector('.translated').classList.add('pending');
  } else if (event.type === 'tts.request') {
    const item = row('user', event.segment_id); item.querySelector('.original').textContent = event.source_text;
    item.querySelector('.translated').textContent = event.text; item.querySelector('.translated').classList.remove('pending');
    item.querySelector('.meta').textContent = `확정 번역 · ${event.model ?? ''}`;
    if ($('live-speak').checked) speech.enqueue(event);
  }
  $('live-conversation').scrollTop = $('live-conversation').scrollHeight;
}
async function play(wav, signal, effect) {
  signal.throwIfAborted(); const context = output;
  const buffer = await context.decodeAudioData(wav); signal.throwIfAborted();
  await new Promise((resolve, reject) => {
    const source = context.createBufferSource(); source.buffer = buffer; source.connect(context.destination);
    let finished = false;
    const stop = () => { try { source.stop(); } catch {} finish(true); };
    function finish(cancelled = false) {
      if (finished) return; finished = true;
      signal.removeEventListener('abort', stop); source.disconnect();
      log({ type: cancelled ? 'playback.cancelled' : 'playback.completed', track: 'user', text: effect.text, model: effect.model });
      row('user', effect.segment_id).querySelector('.meta').textContent = cancelled ? '일본어 음성 중단됨' : '일본어 음성 재생 완료';
      resolve();
    }
    source.onended = () => finish(signal.aborted); signal.addEventListener('abort', stop, { once: true });
    if (signal.aborted) return stop();
    try { source.start(); log({ type: 'playback.started', track: 'user', text: effect.text, model: effect.model }); }
    catch (error) { signal.removeEventListener('abort', stop); source.disconnect(); reject(error); }
  });
}
function stopOutput() {
  if (!running) return;
  speech.clear(); captures.user?.buffer.discard();
  send({ type: 'user.stop_output', command_id: `stop-${++stopId}` });
}
async function end(message = '') {
  const token = ++epoch; running = starting = false;
  const previous = captures; captures = {};
  speech?.close(); speech = null;
  socket?.close(); socket = null;
  const previousStreams = streams; streams = []; const previousOutput = output; output = null;
  previousStreams.forEach(stream => stream.getTracks().forEach(track => track.stop()));
  await Promise.allSettled([...Object.values(previous).map(capture => capture.cancel()), ...(previousOutput && previousOutput.state !== 'closed' ? [previousOutput.close()] : [])]);
  if (epoch !== token) return;
  $('live-status').textContent = message ? '통역 중지 · 입력/로그를 확인해 주세요' : '통역 종료';
  if (message) error(message); refresh();
}
async function microphone(id) {
  return navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true, ...(id ? { deviceId: { exact: id } } : {}) } });
}
async function start() {
  const token = ++epoch; starting = true; error(''); refresh();
  $('live-status').textContent = '오디오 입력 연결 중…';
  try {
    const take = async pending => {
      const stream = await pending;
      if (token !== epoch) { stream.getTracks().forEach(track => track.stop()); throw new DOMException('Cancelled', 'AbortError'); }
      streams.push(stream); return stream;
    };
    const mode = $('live-mode').value, inputs = {};
    // 탭 공유 창은 사용자가 클릭한 직후에만 열 수 있다. 마이크 권한을 기다리기 전에 공유부터 요청한다.
    let sharing;
    if (mode === 'duplex-tab') {
      if (!navigator.mediaDevices.getDisplayMedia) throw new Error('이 브라우저는 탭 오디오 공유를 지원하지 않습니다. 일본어 입력 장치를 선택해 주세요.');
      sharing = take(navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }));
      sharing.catch(() => {});
    }
    output = new AudioContext(); await output.resume();
    if (token !== epoch) return;
    if (sharing) {
      inputs.shop = await sharing;
      if (!inputs.shop.getAudioTracks().length) throw new Error('선택한 화면에 오디오가 없습니다. 일본어 탭과 탭 오디오 공유를 선택해 주세요.');
    }
    if (mode !== 'shop') inputs.user = await take(microphone($('user-device').value));
    if (mode === 'shop' || mode === 'duplex-mics') inputs.shop = await take(microphone($('shop-device').value));
    if (token !== epoch) return;
    if (inputs.user && inputs.shop && mode === 'duplex-mics' && inputs.user.getAudioTracks()[0].getSettings().deviceId === inputs.shop.getAudioTracks()[0].getSettings().deviceId) throw new Error('양방향 통역은 서로 다른 입력 장치가 필요합니다. 같은 마이크는 한 방향 모드에서 사용해 주세요.');
    await devices();
    if (token !== epoch) return;
    const ws = socket = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/live`);
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => { ws.close(); reject(new Error('연속 통역 서비스 연결 시간이 초과됐습니다.')); }, 10000);
      ws.onmessage = event => {
        if (token !== epoch) return;
        const message = JSON.parse(event.data);
        if (message.type === 'ready') { clearTimeout(timer); resolve(); } else render(message);
      };
      ws.onerror = () => { clearTimeout(timer); reject(new Error('연속 통역 서비스 연결에 실패했습니다. Gemma와 새 STT 서버를 확인해 주세요.')); };
      ws.onclose = () => { clearTimeout(timer); reject(new Error('연속 통역 연결이 종료됐습니다.')); if (token === epoch) end('연속 통역 연결이 종료됐습니다.'); };
    });
    if (token !== epoch) return;
    speech = new SpeechQueue({ play, error: cause => end(cause.message), synthesize: async (text, signal) => {
      const response = await fetch('/api/speak', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }), signal });
      if (!response.ok) throw new Error('일본어 음성 합성에 실패했습니다.'); return response.arrayBuffer();
    } });
    rows.clear(); trace = []; $('live-conversation').replaceChildren(); $('live-trace').textContent = '';
    for (const [track, stream] of Object.entries(inputs)) {
      const capture = await captureContinuous({ stream, track, onError: message => { if (token === epoch) end(message); }, onLevel: level => { if (token === epoch) $(`${track}-level`).style.width = `${Math.round(level * 100)}%`; }, onWindow: (meta, wav) => {
        if (token !== epoch || ws.readyState !== 1) return;
        if (ws.bufferedAmount > 1000000) return end('오디오 전송이 밀리고 있습니다. 통역을 다시 시작해 주세요.');
        ws.send(JSON.stringify({ ...meta, wav: audioBase64(wav) }));
      } });
      if (token !== epoch) { await capture.cancel(); return; }
      captures[track] = capture;
    }
    running = true; starting = false; $('live-status').textContent = '계속 듣고 있어요'; refresh();
  } catch (cause) { if (token === epoch) await end(cause.name === 'NotAllowedError' ? '마이크 또는 오디오 공유 권한을 허용해 주세요.' : cause.message); }
}
$('live-start').addEventListener('click', start); $('live-end').addEventListener('click', () => end());
$('live-stop-output').addEventListener('click', stopOutput); $('live-mode').addEventListener('change', refresh);
$('live-check').addEventListener('click', health); $('live-speak').addEventListener('change', () => { if (!$('live-speak').checked) stopOutput(); });
window.addEventListener('pagehide', () => end());
await devices(); await health();
