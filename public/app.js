const $ = id => document.getElementById(id);
let state = null;
let events;
let sending = false;
let scenarios = [];
let renderedMessages = '';
let pendingCreate = null;
const ended = new Set(['completed', 'busy', 'no-answer', 'failed', 'canceled']);
const labels = { draft: '예약 전', discussing: '대화 중', proposed: '대체 시간 제안', reviewing: '예약 내용 확인 중', awaiting_confirmation: '마지막 확인 대기', confirmed: '예약 확정', unavailable: '예약 불가', cancelled: '예약 취소', uncertain: '예약 미확인' };
const actionLabels = { request: '예약 요청', accept: '제안 시간 수락', decline: '다른 시간 문의', name: '이름 전달', confirm_details: '예약 내용 확인', repeat: '다시 말씀해 주세요', wait: '잠시만요', thanks: '감사합니다' };

function notice(message = '') { $('notice').textContent = message; $('notice').hidden = !message; }
async function api(path, body, headers = {}) {
  const response = await fetch(path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body: JSON.stringify(body) });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || '요청을 처리하지 못했습니다.');
  return data;
}

function dateLabel(date) {
  return new Intl.DateTimeFormat('ko-KR', { month: 'long', day: 'numeric', weekday: 'short', timeZone: 'Asia/Tokyo' }).format(new Date(`${date}T12:00:00+09:00`));
}

function apply(next) {
  if (state && next.id === state.id && next.revision < state.revision) return;
  state = next;
  render();
}

function connect(id) {
  events?.close();
  events = new EventSource(`/api/calls/${id}/events`);
  events.addEventListener('snapshot', event => { const next = JSON.parse(event.data); if (next.id === state?.id) apply(next); });
  events.onerror = () => { $('footer-status').textContent = '상태 연결을 다시 시도하고 있어요.'; };
}

function japaneseVoice() { return window.speechSynthesis?.getVoices().find(voice => voice.lang.toLowerCase().startsWith('ja')); }
function speak(text) {
  const voice = japaneseVoice();
  if (!voice) return notice('이 브라우저에 일본어 음성이 없습니다. 일본어 원문으로 확인해 주세요.');
  const utterance = new SpeechSynthesisUtterance(text);
  utterance.lang = 'ja-JP'; utterance.voice = voice; utterance.rate = 0.9;
  utterance.onerror = () => notice('일본어 음성을 재생하지 못했습니다. 원문으로 확인해 주세요.');
  window.speechSynthesis.cancel(); window.speechSynthesis.speak(utterance);
}

function create(tag, className, text) { const element = document.createElement(tag); if (className) element.className = className; if (text !== undefined) element.textContent = text; return element; }
function renderMessages() {
  const key = JSON.stringify(state.messages);
  if (key === renderedMessages) return;
  renderedMessages = key;
  const body = $('conversation-body');
  const atBottom = body.scrollTop + body.clientHeight >= body.scrollHeight - 70;
  const fragment = document.createDocumentFragment();
  for (const message of state.messages) {
    const row = create('article', `message ${message.role}`);
    row.append(create('span', 'avatar', message.role === 'employee' ? 'さ' : '나'));
    const content = create('div', 'message-content');
    content.append(create('div', 'message-label', message.role === 'employee' ? '사쿠라 식당 · 한국어 자막' : '나 · 한국어 답변'));
    content.append(create('div', 'message-bubble', message.role === 'employee' ? message.translated : message.original));
    const meta = create('div', 'message-meta');
    const details = create('details');
    details.append(create('summary', '', message.role === 'employee' ? '일본어 원문' : '일본어 전달문'));
    const japanese = message.role === 'employee' ? message.original : message.translated;
    const original = create('p', '', japanese); original.lang = 'ja'; details.append(original);
    const listen = create('button', 'listen-button', '▷ 일본어 듣기'); listen.type = 'button'; listen.title = '브라우저 음성 미리듣기 (통화 미디어와 별도)'; listen.disabled = !japaneseVoice(); listen.addEventListener('click', () => speak(japanese));
    meta.append(details, listen); content.append(meta);
    if (message.role === 'user') content.append(create('div', 'delivery-status', { sending: '모의 전달 중', delivered: '시나리오에 전달됨', cancelled: '전달 취소', failed: '전달 실패' }[message.status] || ''));
    row.append(content); fragment.append(row);
  }
  $('messages').replaceChildren(fragment);
  if (atBottom || state.messages.length <= 2) body.scrollTop = body.scrollHeight;
}

function render() {
  const active = state && !ended.has(state.status);
  const connected = state?.status === 'in-progress';
  for (const input of document.querySelectorAll('[data-lock]')) input.disabled = Boolean(active || sending);
  $('start-call').disabled = Boolean(active || sending);
  $('start-call').textContent = active ? (connected ? '모의 통화 진행 중' : '연결하는 중…') : state ? '다시 연습하기' : '☎ 모의 통화 시작';
  $('end-call').disabled = !active;
  if (!state) return updateSummary();
  const ready = connected && state.suggestions.length > 0 && !state.replyBusy && !sending;
  $('reply').disabled = !ready;
  $('send-reply').disabled = !ready || !$('reply').value.trim();
  $('reply').placeholder = ready ? '추천 답변을 선택하거나 같은 문장을 입력하세요.' : ended.has(state.status) ? '통화가 종료되었습니다.' : '직원의 답변을 기다려 주세요.';
  $('stop-reply').disabled = !state.pendingReply;
  $('empty-state').hidden = state.messages.length > 0;
  if (!state.messages.length) {
    $('empty-state').querySelector('h3').textContent = state.status === 'busy' ? '지금은 통화 중이에요.' : state.status === 'no-answer' ? '전화를 받지 않았어요.' : state.status === 'failed' ? '연결하지 못했어요.' : ended.has(state.status) ? '통화가 종료되었어요.' : '식당에 연결하고 있어요.';
    $('empty-state').querySelector('p').textContent = ended.has(state.status) ? '다른 시나리오를 선택하거나 다시 연습해 보세요.' : '로컬 모의 서버가 통화와 직원의 답변을 준비하고 있어요.';
  }
  const status = $('call-status'); status.className = `call-status ${connected ? 'connected' : ended.has(state.status) ? 'ended' : ''}`; status.querySelector('span').textContent = state.statusLabel || state.status;
  $('waiting').hidden = !(connected && state.replyBusy);
  $('waiting-text').textContent = state.pendingReply ? '일본어 답변을 시나리오에 전달하고 있어요.' : '직원이 답변을 준비하고 있어요.';
  $('footer-status').textContent = ended.has(state.status) ? (state.reservation.status === 'confirmed' ? '예약 내용을 확인했어요. 연습이 완료되었습니다.' : '통화가 끝났어요. 예약 확정 여부를 확인하세요.') : connected ? '한국어 답변을 일본어 문장으로 전달해요.' : '실제 전화망에 연결하지 않는 모의 통화입니다.';
  const fragment = document.createDocumentFragment();
  for (const suggestion of state.suggestions) {
    const button = create('button', 'suggestion', actionLabels[suggestion.action] || suggestion.action); button.type = 'button'; button.disabled = !ready; button.title = suggestion.ko;
    button.addEventListener('click', () => { $('reply').value = suggestion.ko; $('send-reply').disabled = false; $('reply').focus(); }); fragment.append(button);
  }
  $('suggestions').replaceChildren(fragment);
  renderMessages(); updateSummary();
  const m = state.metrics; $('media-count').textContent = `수신 ${m.inboundFrames || 0} · 송신 ${m.outboundFrames || 0} · mark ${m.marks || 0}`;
  $('protocol-log').textContent = state.trace.map(e => `${e.at.slice(11, 19)}  ${e.direction.padEnd(11)} ${e.event}${e.detail ? `  ${e.detail}` : ''}`).join('\n') || '미디어 연결을 기다리는 중입니다.';
  $('protocol-log').scrollTop = $('protocol-log').scrollHeight;
}

function updateSummary() {
  const r = state?.reservation || Object.fromEntries(new FormData($('reservation-form')));
  $('summary-date').textContent = r.date ? dateLabel(r.date) : '—';
  const time = $('summary-time'); time.replaceChildren();
  if (r.proposedTime && r.proposedTime !== r.time) time.append(create('del', '', r.time));
  time.append(document.createTextNode(r.proposedTime || r.time || '—'));
  $('summary-party').textContent = `${r.partySize || 2}명`; $('summary-name').textContent = r.name || '—';
  const badge = $('reservation-status'); badge.textContent = labels[r.status] || '예약 전'; badge.className = `reservation-badge ${r.status || ''}`;
  const confirm = $('confirm-reservation'); confirm.disabled = !state?.employeeConfirmed || r.status === 'confirmed';
  confirm.textContent = r.status === 'confirmed' ? '✓ 예약 내용을 확인했어요' : state?.employeeConfirmed ? '이 내용으로 예약 확정' : '직원 확인 후 예약 확정';
  $('confirmation-note').textContent = r.status === 'confirmed' ? '직원과 날짜, 시간, 인원, 이름을 확인했어요. 실제 예약은 이루어지지 않았습니다.' : state?.employeeConfirmed ? '직원이 예약을 접수했어요. 위 내용이 맞으면 직접 확정해 주세요.' : '직원의 확인을 받은 뒤, 내용이 맞는지 한 번 더 확인해 주세요.';
}

$('reservation-form').addEventListener('input', () => { if (!state) updateSummary(); });
$('reservation-form').addEventListener('submit', async event => {
  event.preventDefault(); notice(); sending = true; render();
  const reservation = Object.fromEntries(new FormData($('reservation-form')));
  // 비활성화한 필드는 FormData에서 빠지므로 잠긴 입력란의 값은 직접 읽는다.
  for (const input of $('reservation-form').querySelectorAll('[name]')) reservation[input.name] = input.value;
  reservation.scenario = $('scenario').value;
  const fingerprint = JSON.stringify(reservation);
  if (pendingCreate?.fingerprint !== fingerprint) pendingCreate = { fingerprint, key: crypto.randomUUID() };
  try {
    const next = await api('/api/calls', reservation, { 'Idempotency-Key': pendingCreate.key });
    pendingCreate = null;
    state = null; renderedMessages = ''; $('reply').value = ''; apply(next); sessionStorage.setItem('yoyaku-call', next.id); connect(next.id);
  } catch (error) { notice(error.message); }
  finally { sending = false; render(); }
});

async function sendReply(event) {
  event?.preventDefault();
  if (!state || sending || state.replyBusy || !$('reply').value.trim()) return;
  sending = true; notice(); render();
  try { apply(await api(`/api/calls/${state.id}/replies`, { text: $('reply').value, expected_reservation_revision: state.reservation.revision })); $('reply').value = ''; }
  catch (error) { notice(error.message); }
  finally { sending = false; render(); }
}
$('reply-form').addEventListener('submit', sendReply);
$('reply').addEventListener('input', () => { $('send-reply').disabled = !$('reply').value.trim() || sending || state?.replyBusy; });
$('reply').addEventListener('keydown', event => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') sendReply(event); });
for (const [id, action] of [['end-call', 'end'], ['stop-reply', 'stop'], ['confirm-reservation', 'confirm']]) {
  $(id).addEventListener('click', async () => {
    if (!state) return;
    notice(); $(id).disabled = true;
    if (action === 'end') window.speechSynthesis?.cancel();
    try { apply(await api(`/api/calls/${state.id}/${action}`, action === 'confirm' ? { expected_reservation_revision: state.reservation.revision } : {})); }
    catch (error) { notice(error.message); render(); }
  });
}
$('guide-open').addEventListener('click', () => $('guide').showModal());
for (const id of ['guide-close', 'guide-done']) $(id).addEventListener('click', () => $('guide').close());
$('scenario').addEventListener('change', () => { $('scenario-description').textContent = scenarios.find(item => item.id === $('scenario').value)?.description || ''; });
window.speechSynthesis?.addEventListener('voiceschanged', () => { for (const button of document.querySelectorAll('.listen-button')) button.disabled = !japaneseVoice(); });

async function init() {
  const today = new Date(); today.setDate(today.getDate() + 7);
  $('date').value = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Tokyo', year: 'numeric', month: '2-digit', day: '2-digit' }).format(today);
  updateSummary();
  try {
    const config = await api('/api/config'); scenarios = config.scenarios;
    for (const scenario of scenarios) { const option = create('option', '', `${scenario.title} · ${scenario.tag}`); option.value = scenario.id; $('scenario').append(option); }
    $('scenario').dispatchEvent(new Event('change'));
    const previous = sessionStorage.getItem('yoyaku-call');
    if (previous) {
      try {
        const restored = await api(`/api/calls/${previous}`);
        for (const input of $('reservation-form').querySelectorAll('[name]')) if (restored.reservation[input.name] !== undefined) input.value = restored.reservation[input.name];
        $('scenario').value = restored.scenario; $('scenario').dispatchEvent(new Event('change')); apply(restored); connect(previous);
      } catch { sessionStorage.removeItem('yoyaku-call'); }
    }
  } catch (error) { notice(error.message); $('start-call').disabled = true; }
}
init();
