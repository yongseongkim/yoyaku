export function pcmWav(chunks, inputRate) {
  const length = chunks.reduce((sum, chunk) => sum + chunk.length, 0);
  const source = new Float32Array(length);
  let offset = 0;
  for (const chunk of chunks) { source.set(chunk, offset); offset += chunk.length; }
  const count = Math.floor(length * 16000 / inputRate);
  const buffer = new ArrayBuffer(44 + count * 2), view = new DataView(buffer);
  const label = (start, text) => [...text].forEach((char, i) => view.setUint8(start + i, char.charCodeAt(0)));
  label(0, 'RIFF'); view.setUint32(4, buffer.byteLength - 8, true); label(8, 'WAVE'); label(12, 'fmt ');
  view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, 16000, true); view.setUint32(28, 32000, true); view.setUint16(32, 2, true); view.setUint16(34, 16, true);
  label(36, 'data'); view.setUint32(40, count * 2, true);
  for (let i = 0; i < count; i++) {
    const position = i * inputRate / 16000, a = Math.floor(position), fraction = position - a;
    const sample = Math.max(-1, Math.min(1, source[a] * (1 - fraction) + (source[a + 1] ?? source[a]) * fraction));
    view.setInt16(44 + i * 2, sample < 0 ? sample * 32768 : sample * 32767, true);
  }
  return buffer;
}

export async function recordVoice({ autoStop, onLevel, onStop }) {
  if (!navigator.mediaDevices?.getUserMedia || !window.AudioContext) throw new Error('이 브라우저에서는 마이크를 사용할 수 없습니다. Chrome 또는 Safari에서 localhost 화면을 열어 주세요.');
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  let context, source, node, timer, ended = false;
  const chunks = []; let frames = 0, voicedMs = 0, lastVoice = null;
  async function finish(cancel = false) {
    if (ended) return;
    ended = true; clearTimeout(timer);
    source?.disconnect(); node?.disconnect(); stream.getTracks().forEach(track => track.stop());
    if (context) await context.close();
    onLevel(0);
    if (cancel) return;
    if (voicedMs < 200) return onStop(null, '목소리가 들리지 않았습니다. 마이크를 확인하고 다시 말해 주세요.');
    onStop(pcmWav(chunks, context.sampleRate));
  }
  try {
    context = new AudioContext({ sampleRate: 16000 });
    await context.audioWorklet.addModule('/recorder-worklet.js');
    source = context.createMediaStreamSource(stream); node = new AudioWorkletNode(context, 'voice-recorder');
    node.port.onmessage = event => {
      if (ended) return;
      const samples = event.data; chunks.push(samples); frames += samples.length;
      const elapsed = frames / context.sampleRate * 1000;
      const rms = Math.sqrt(samples.reduce((sum, value) => sum + value * value, 0) / samples.length);
      onLevel(Math.min(1, rms * 10));
      if (rms >= 0.012) { voicedMs += samples.length / context.sampleRate * 1000; lastVoice = elapsed; }
      if (elapsed >= 19900 || autoStop() && voicedMs >= 200 && elapsed - lastVoice >= 900) finish();
    };
    source.connect(node); node.connect(context.destination); await context.resume();
    timer = setTimeout(() => finish(), 20000);
    return { stop: () => finish(), cancel: () => finish(true) };
  } catch (error) { await finish(true); throw error; }
}
