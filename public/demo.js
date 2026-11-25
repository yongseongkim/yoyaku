import { recordVoice } from './voice-recorder.js';

const $ = id => document.getElementById(id);
let scenarios = [], scenario, index = -1, epoch = 0, busy = false, answered = false;
let recorder, audio, audioUrl, controller = new AbortController(), lastShop = null, services = {};
let models = [], modelState = {}, managed = false, modelSwitching = false;
let manualRecords = [], checkedItems = new Set();
const step = () => scenario?.steps[index];
const current = token => token === epoch;
function showError(message = '') { $('error').textContent = message; $('error').hidden = !message; }
function status(text) { $('prompt').textContent = text; }
function refresh() {
  const user = step()?.speaker === 'user', active = index >= 0;
  $('mic').disabled = !user || busy || !services.speech;
  $('translate').disabled = !user || busy || Boolean(recorder);
  $('audio-file').disabled = !user || busy || Boolean(recorder) || !services.speech;
  $('next').disabled = !active || index >= scenario.steps.length || busy || Boolean(recorder) || (user && !answered);
  $('repeat').disabled = !lastShop || busy || Boolean(recorder);
  $('progress').textContent = !active ? '준비' : index >= scenario.steps.length ? '연습 완료' : `${index + 1} / ${scenario.steps.length} 구간`;
  $('mic').classList.toggle('recording', Boolean(recorder));
  $('mic').innerHTML = recorder ? '<span aria-hidden="true">■</span> 말하기 완료' : '<span aria-hidden="true">●</span> 마이크로 답하기';
  $('hint').hidden = !user;
  if (user) $('hint').textContent = `답변 예시: ${step().example} · 다른 답변도 가능해요.`;
  $('start').textContent = active ? '처음부터 다시' : '연습 시작';
  $('start').disabled = !services.translation || !scenarios.length || modelSwitching;
  $('scenario').disabled = modelSwitching || !scenarios.length;
  $('check').disabled = busy || Boolean(recorder) || modelSwitching;
  $('model').disabled = !managed || busy || Boolean(recorder) || modelSwitching;
  $('switch-model').disabled = $('model').disabled || !$('model').value || $('model').value === modelState.active;
  $('export-session').disabled = !manualRecords.length;
}
function renderCase() {
  if (!scenario) return;
  $('case-category').textContent = `${scenario.category} · ${scenario.id}`;
  $('case-title').textContent = scenario.title;
  $('case-description').textContent = scenario.description;
  $('case-checks').replaceChildren(...scenario.checks.map((text, index) => {
    const label = document.createElement('label'); label.className = 'checkbox';
    const input = document.createElement('input'); input.type = 'checkbox'; input.checked = checkedItems.has(index);
    input.addEventListener('change', () => { if (input.checked) checkedItems.add(index); else checkedItems.delete(index); updateChecks(); });
    label.append(input, document.createTextNode(text)); return label;
  })); updateChecks();
}
function updateChecks() { $('case-check-progress').textContent = `직접 확인 ${checkedItems.size} / ${scenario?.checks.length ?? 0}`; }
function selectCase() {
  reset(); scenario = scenarios.find(s => s.id === $('scenario').value); renderCase();
  const empty = document.createElement('p'); empty.className = 'empty'; empty.textContent = '선택한 케이스의 확인 항목을 읽고 연습 시작을 눌러 주세요.';
  $('conversation').replaceChildren(empty);
  const url = new URL(location.href); url.searchParams.set('scenario', scenario.id); history.replaceState(null, '', url);
  status('연습 시작을 눌러 주세요.'); refresh();
}
async function request(path, body, signal = controller.signal) {
  const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || '요청에 실패했습니다.');
  return result;
}
function message(role, original, sourceLabel = '') {
  $('conversation').querySelector('.empty')?.remove();
  const item = document.createElement('article'); item.className = `message ${role}`;
  item.caseStep = { id: step()?.id, reference: step()?.reference };
  for (const [className, text] of [['role', role === 'shop' ? 'A · 점원 / 日本語 → 한국어' : `B · 나 / 한국어 → 日本語${sourceLabel}`], ['original', original], ['translated pending', '번역 중…'], ['meta', '']]) {
    const node = document.createElement('p'); node.className = className; node.textContent = text;
    if (className === 'original') node.lang = role === 'shop' ? 'ja' : 'ko';
    if (className.startsWith('translated')) node.lang = role === 'shop' ? 'ko' : 'ja';
    item.append(node);
  }
  $('conversation').append(item); scroll(); return item;
}
function scroll() { const pane = $('conversation'); pane.scrollTop = pane.scrollHeight; }
function translated(item, result, stt) {
  const target = item.querySelector('.translated'); target.textContent = result.text; target.classList.remove('pending');
  const timing = stt ? `음성 인식 ${stt.latency_ms}ms · 번역 ${result.latency_ms}ms · 녹음 ${(stt.audio_duration_ms / 1000).toFixed(1)}초` : `번역 ${result.latency_ms}ms`;
  item.querySelector('.meta').textContent = timing + (result.model ? ` · ${models.find(model => model.id === result.model)?.name ?? result.model}` : '');
  if (item.caseStep.reference && !item.querySelector('details')) {
    const details = document.createElement('details'), summary = document.createElement('summary'), reference = document.createElement('p');
    summary.textContent = item.classList.contains('shop') ? '기대 의미 보기 · 사람이 비교하는 참고' : '답변 예시의 일본어 참고 · 다른 답변도 가능';
    reference.textContent = item.caseStep.reference; details.append(summary, reference); item.append(details);
  }
  manualRecords.push({ step_id: item.caseStep.id, speaker: item.classList.contains('shop') ? 'A' : 'B', input_mode: stt ? 'microphone_or_wav' : item.classList.contains('shop') ? 'script' : 'typed', source: item.querySelector('.original').textContent, translated: result.text, reference: item.caseStep.reference, model: result.model, prompt_version: result.prompt_version, translation_latency_ms: result.latency_ms, stt: stt ? { text: stt.text, model: stt.model, prompt_version: stt.prompt_version, latency_ms: stt.latency_ms } : undefined, at: new Date().toISOString() });
  scroll();
}
function stopAudio() {
  if (audio) { audio.pause(); audio.dispatchEvent(new Event('rehearsal-stop')); audio = null; }
  if (audioUrl) { URL.revokeObjectURL(audioUrl); audioUrl = null; }
  $('stop-audio').disabled = true;
}
async function speak(text, token) {
  const response = await fetch('/api/speak', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }), signal: controller.signal });
  if (!response.ok) { const result = await response.json(); throw new Error(result.error || '음성을 만들지 못했습니다.'); }
  const blob = await response.blob();
  if (!current(token)) return;
  stopAudio(); audioUrl = URL.createObjectURL(blob); const playing = audio = new Audio(audioUrl);
  $('stop-audio').disabled = false;
  try {
    await new Promise((resolve, reject) => {
      playing.addEventListener('ended', resolve, { once: true });
      playing.addEventListener('rehearsal-stop', resolve, { once: true });
      playing.addEventListener('error', () => reject(new Error('음성 재생을 실패했습니다. 다시 듣기를 눌러 주세요.')), { once: true });
      playing.play().catch(() => reject(new Error('브라우저가 음성 재생을 막았습니다. 다시 듣기를 눌러 주세요.')));
    });
  } finally { if (current(token) && audio === playing) stopAudio(); }
}
async function runShop() {
  const source = step(); if (!source || source.speaker !== 'shop') return;
  const token = epoch, item = message('shop', source.text); lastShop = source.text;
  busy = true; showError(); status('점원이 말하고 있어요. 한국어 자막을 확인해 주세요.'); refresh();
  const results = await Promise.allSettled([
    request('/api/translate', { track: 'shop', text: source.text }).then(result => { if (current(token)) translated(item, result); }),
    speak(source.text, token),
  ]);
  if (!current(token)) return;
  const errors = results.filter(r => r.status === 'rejected');
  if (errors.length) {
    showError(errors.map(r => r.reason.message).join(' '));
    if (results[0].status === 'rejected') item.querySelector('.translated').textContent = '자막 번역 실패';
  }
  busy = false; index++; answered = false;
  status(step()?.speaker === 'user' ? '이제 한국어로 답해 보세요.' : step() ? '다음 문구를 눌러 계속 들어보세요.' : '연습을 마쳤습니다. 원문과 번역을 비교해 보세요.'); refresh();
}
async function submitText(text, stt, token = epoch) {
  text = text.trim(); if (!text || !current(token)) return;
  const item = message('user', text, stt ? ' · 마이크/녹음 인식' : ' · 직접 입력');
  busy = true; answered = false; showError(); status('내 말을 일본어로 번역하고 있어요.'); refresh();
  try {
    const result = await request('/api/translate', { track: 'user', text });
    if (!current(token)) return;
    translated(item, result, stt); answered = true;
    if ($('speak-reply').checked) { status('일본어 답변을 읽어드려요.'); await speak(result.text, token); }
    if (current(token)) status('번역을 확인하고 다음 문구로 넘어가세요. 같은 질문에 다시 답해도 괜찮아요.');
  } catch (error) {
    if (current(token)) { showError(error.message); if (!answered) item.querySelector('.translated').textContent = '번역 실패 · 다시 시도해 주세요.'; }
  } finally { if (current(token)) { busy = false; refresh(); } }
}
async function submitAudio(wav, token) {
  if (!current(token)) return;
  busy = true; showError(); status('한국어 음성을 인식하고 있어요.'); $('record-status').textContent = '인식 중…'; refresh();
  try {
    const response = await fetch('/api/transcribe', { method: 'POST', headers: { 'Content-Type': 'audio/wav' }, body: wav, signal: controller.signal });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || '음성 인식에 실패했습니다.');
    if (!current(token)) return;
    $('typed').value = result.text;
    await submitText(result.text, result, token);
  } catch (error) { if (current(token)) { showError(error.message); status('다시 녹음하거나 한국어를 직접 입력해 주세요.'); } }
  finally { if (current(token)) { busy = false; $('record-status').textContent = '녹음 대기'; refresh(); } }
}
async function toggleMic() {
  if (recorder) { const previous = recorder; recorder = null; busy = true; refresh(); await previous.stop(); return; }
  const token = epoch; busy = true; showError(); status('마이크 권한을 확인하고 있어요.'); refresh();
  try {
    const captured = await recordVoice({
      autoStop: () => $('auto-stop').checked,
      onLevel: level => { if (current(token)) $('meter').style.width = `${Math.round(level * 100)}%`; },
      onStop: (wav, error) => {
        if (!current(token)) return;
        recorder = null; $('record-status').textContent = '녹음 대기';
        if (error) { busy = false; showError(error); status('마이크를 확인하고 다시 말해 주세요.'); refresh(); }
        else submitAudio(wav, token);
      },
    });
    if (!current(token)) { await captured.cancel(); return; }
    recorder = captured; busy = false; $('record-status').textContent = '듣고 있어요';
    status('한국어로 말해 주세요. 말을 마치면 자동으로 인식하거나 완료 버튼을 누르세요.'); refresh();
  } catch (error) {
    if (current(token)) { busy = false; showError(error.name === 'NotAllowedError' ? '마이크 권한이 필요합니다. 주소창의 권한 설정에서 마이크를 허용해 주세요.' : error.message); status('마이크를 사용할 수 없으면 아래에서 직접 입력할 수 있어요.'); refresh(); }
  }
}
function reset() {
  epoch++; controller.abort(); controller = new AbortController();
  recorder?.cancel(); recorder = null; stopAudio(); busy = false; answered = false; lastShop = null; index = -1;
  $('record-status').textContent = '녹음 대기'; $('typed').value = ''; $('meter').style.width = '0%'; showError();
  manualRecords = []; checkedItems.clear();
}
async function checkHealth() {
  $('health').textContent = '연결 확인 중…'; $('check').disabled = true;
  try {
    const response = await fetch('/api/health'); services = await response.json();
    $('health').textContent = `번역 ${services.translation ? `연결됨 (${services.model || '로컬'})` : '꺼짐'} · 음성 인식 ${services.speech ? '연결됨' : '꺼짐'} · Mac 일본어 음성`;
    if (!services.translation || !services.speech) showError('로컬 서비스가 꺼져 있어요. ./yoyaku serve를 실행하고 다시 확인을 눌러 주세요.');
    else showError();
  } catch { showError('시연 서버 연결을 확인해 주세요.'); $('health').textContent = '연결 실패'; }
  await loadModels(); refresh();
}
async function loadModels() {
  try {
    const response = await fetch('/api/models'); const data = await response.json();
    if (!response.ok) throw new Error(data.error);
    modelState = data; models = data.models; managed = true;
    const chosen = data.active ?? data.target ?? models.find(model => model.id === $('model').value && model.installed)?.id ?? data.default;
    $('model').replaceChildren(...models.map(model => {
      const option = document.createElement('option'); option.value = model.id;
      option.textContent = `${model.name}${model.default ? ' · 기본' : ''}${model.translation_only ? ' · 번역 전용' : ''}${model.installed ? '' : ' · 미설치'}`;
      option.disabled = !model.installed; return option;
    }));
    $('model').value = chosen;
    $('model-status').textContent = data.state === 'ready' ? `현재 모델: ${models.find(model => model.id === data.active)?.name ?? data.active} · 인식·번역·재생이 끝난 뒤 바꿀 수 있어요.`
      : data.state === 'loading' || data.state === 'stopping' ? '모델을 바꾸고 있어요. 잠시 후 다시 확인해 주세요.' : data.error ?? '모델을 선택하고 적용해 주세요.';
  } catch { managed = false; $('model-status').textContent = '모델 선택을 사용하려면 ./yoyaku serve를 실행해 주세요.'; }
}
async function switchModel() {
  if (busy || recorder || !managed || $('model').value === modelState.active) return;
  const profile = $('model').value, token = epoch;
  modelSwitching = true; busy = true; showError(); refresh();
  $('model-status').textContent = '현재 모델을 내리고 선택한 모델을 불러오고 있어요…';
  try {
    await request('/api/models/switch', { profile });
    if (!current(token)) return;
    await checkHealth();
    $('model').value = modelState.active;
    status('모델을 바꿨습니다. 이어서 연습하거나 처음부터 시작해 보세요.');
  } catch (error) {
    if (current(token)) { showError(error.message); await loadModels(); }
  } finally { if (current(token)) { modelSwitching = false; busy = false; refresh(); } }
}
$('model').addEventListener('change', refresh);
$('switch-model').addEventListener('click', switchModel);
$('mic').addEventListener('click', toggleMic);
$('stop-audio').addEventListener('click', stopAudio);
$('check').addEventListener('click', checkHealth);
$('scenario').addEventListener('change', selectCase);
$('start').addEventListener('click', async () => {
  reset(); scenario = scenarios.find(s => s.id === $('scenario').value); index = 0; $('conversation').replaceChildren();
  renderCase();
  if (step().speaker === 'shop') await runShop(); else { status('먼저 한국어로 예약 요청을 말해 보세요.'); refresh(); }
});
$('next').addEventListener('click', async () => {
  if (index >= scenario.steps.length) { status('연습 완료! 다른 상황을 고르거나 처음부터 다시 시작해 보세요.'); return; }
  if (step().speaker === 'user') index++;
  answered = false; $('typed').value = '';
  if (step()?.speaker === 'shop') await runShop();
  else { status(step() ? '한국어로 답해 주세요.' : '연습을 마쳤습니다. 원문과 번역을 비교해 보세요.'); refresh(); }
});
$('repeat').addEventListener('click', async () => {
  const token = epoch; busy = true; showError(); refresh();
  try { await speak(lastShop, token); } catch (error) { if (current(token)) showError(error.message); }
  finally { if (current(token)) { busy = false; refresh(); } }
});
$('text-form').addEventListener('submit', event => { event.preventDefault(); if (!busy && !recorder && step()?.speaker === 'user') submitText($('typed').value); });
$('audio-file').addEventListener('change', async () => {
  const file = $('audio-file').files[0]; if (!file) return;
  const token = epoch; $('audio-file').value = '';
  if (file.size > 640044) return showError('20초 이하의 16kHz 모노 PCM16 WAV가 필요합니다.');
  await submitAudio(await file.arrayBuffer(), token);
});
window.addEventListener('pagehide', reset);
$('export-session').addEventListener('click', () => {
  if (!manualRecords.length) return;
  const data = { schema_version: 1, kind: 'manual-reservation-rehearsal', case_id: scenario.id, title: scenario.title, at: new Date().toISOString(), script_is_fixed: true, automatic_semantic_score: false, checks: scenario.checks.map((text, index) => ({ text, user_checked: checkedItems.has(index) })), records: manualRecords };
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
  const a = document.createElement('a'); a.href = url; a.download = `yoyaku-${scenario.id}-${Date.now()}.json`; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
});
try {
  const response = await fetch('/api/scenarios'); const data = await response.json(); scenarios = data.scenarios;
  const groups = new Map();
  for (const s of scenarios) { if (!groups.has(s.category)) { const group = document.createElement('optgroup'); group.label = s.category; groups.set(s.category, group); } const option = document.createElement('option'); option.value = s.id; option.textContent = s.title; groups.get(s.category).append(option); }
  $('scenario').replaceChildren(...groups.values());
  const requested = new URLSearchParams(location.search).get('scenario');
  $('scenario').value = scenarios.some(s => s.id === requested) ? requested : scenarios[0].id;
  $('scenario').disabled = false; selectCase(); await checkHealth();
} catch { showError('시나리오를 불러오지 못했습니다. 화면을 새로고침해 주세요.'); }
