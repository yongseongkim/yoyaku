import { pcmWav } from './voice-recorder.js';

export class ContinuousAudioBuffer {
  constructor({ track, rate = 16000, onWindow, onLevel = () => {}, onLimit = () => {}, intervalMs = 900, maxMs = 20000 }) {
    Object.assign(this, { track, rate, onWindow, onLevel, onLimit, intervalMs, maxMs });
    this.counter = 1; this.sequence = 0; this.chunks = []; this.pre = []; this.frames = 0;
    this.clock = 0; this.lastVoice = null; this.voicedMs = 0; this.lastSent = -Infinity; this.confirmations = 0;
    this.inFlight = null; this.closed = false;
  }
  get id() { return `${this.track}-${this.counter}`; }
  add(samples) {
    if (this.closed) return;
    const ms = samples.length / this.rate * 1000; this.clock += ms;
    const rms = Math.sqrt(samples.reduce((sum, x) => sum + x * x, 0) / samples.length);
    this.onLevel(Math.min(1, rms * 10));
    const voiced = rms >= 0.012;
    if (!this.frames && !voiced) { this.pre.push(samples); if (this.pre.length > 3) this.pre.shift(); return; }
    if (!this.frames) { this.chunks = this.pre.splice(0); this.frames = this.chunks.reduce((n, chunk) => n + chunk.length, 0); }
    if (voiced) { this.lastVoice = this.clock; this.voicedMs += ms; this.confirmations = 0; }
    if (voiced || this.lastVoice !== null && this.clock - this.lastVoice <= 1000) { this.chunks.push(samples); this.frames += samples.length; }
    if (this.frames / this.rate * 1000 > this.maxMs) { this.closed = true; this.onLimit('20초 안에 안정된 의미 구간을 확정하지 못했습니다. 화면의 원문을 확인하고 통역을 다시 시작해 주세요.'); return; }
    this.pump();
  }
  pump() {
    if (this.closed || this.inFlight || this.frames / this.rate < 0.9 || this.voicedMs < 120 || this.clock - this.lastSent < this.intervalMs) return;
    const quiet = this.lastVoice === null ? 0 : this.clock - this.lastVoice;
    if (quiet >= 900 && this.confirmations >= 2) return;
    if (quiet >= 900) this.confirmations++;
    const wav = pcmWav(this.chunks, this.rate);
    const meta = { type: 'audio', track: this.track, window_id: this.id, sequence: ++this.sequence, samples: (wav.byteLength - 44) / 2, quiet_ms: Math.min(5000, Math.round(quiet)) };
    this.inFlight = { window_id: meta.window_id, sequence: meta.sequence, quiet: quiet >= 900 }; this.lastSent = this.clock;
    this.onWindow(meta, wav);
  }
  ack(id, sequence, reason = 'processed') {
    if (this.inFlight?.window_id === id && this.inFlight.sequence === sequence) {
      if (reason === 'busy' && this.inFlight.quiet) this.confirmations = Math.max(0, this.confirmations - 1);
      this.inFlight = null; this.pump();
    }
  }
  consume(id, throughSamples) {
    if (id !== this.id || !Number.isInteger(throughSamples) || throughSamples < 1 || throughSamples > Math.floor(this.frames * 16000 / this.rate)) return false;
    let remove = Math.min(this.frames, Math.round(throughSamples * this.rate / 16000));
    const left = [];
    for (const chunk of this.chunks) {
      if (remove >= chunk.length) remove -= chunk.length;
      else { left.push(remove ? chunk.slice(remove) : chunk); remove = 0; }
    }
    this.counter++; this.sequence = 0; this.chunks = left; this.frames = left.reduce((n, chunk) => n + chunk.length, 0);
    this.inFlight = null; this.lastSent = -Infinity; this.confirmations = 0;
    this.voicedMs = left.reduce((sum, chunk) => sum + (Math.sqrt(chunk.reduce((s, x) => s + x * x, 0) / chunk.length) >= 0.012 ? chunk.length / this.rate * 1000 : 0), 0);
    if (!this.voicedMs) { this.frames = 0; this.chunks = []; this.lastVoice = null; }
    this.pump(); return true;
  }
  discard(id) {
    if (id && id !== this.id) return;
    this.counter++; this.sequence = 0; this.chunks = []; this.pre = []; this.frames = 0; this.voicedMs = 0;
    this.lastVoice = null; this.inFlight = null; this.confirmations = 0; this.lastSent = -Infinity;
  }
  close() { this.closed = true; this.chunks = []; this.pre = []; this.onLevel(0); }
}

export async function captureContinuous({ stream, track, onWindow, onLevel, onError }) {
  const context = new AudioContext({ sampleRate: 16000 });
  let source, node, ended = false;
  const buffer = new ContinuousAudioBuffer({ track, rate: context.sampleRate, onWindow, onLevel, onLimit: onError });
  const cancel = async () => {
    if (ended) return; ended = true; buffer.close(); source?.disconnect(); node?.disconnect();
    stream.getTracks().forEach(track => track.stop()); await context.close();
  };
  try {
    await context.audioWorklet.addModule('/recorder-worklet.js');
    source = context.createMediaStreamSource(stream); node = new AudioWorkletNode(context, 'voice-recorder');
    node.port.onmessage = event => { try { buffer.add(event.data); } catch (error) { onError(error.message); } };
    for (const audioTrack of stream.getAudioTracks()) audioTrack.addEventListener('ended', () => { if (!ended) onError('오디오 입력이 종료됐습니다. 통역을 다시 시작해 주세요.'); }, { once: true });
    source.connect(node); node.connect(context.destination); await context.resume();
    return { cancel, buffer };
  } catch (error) { await cancel(); throw error; }
}

export function audioBase64(buffer) {
  const bytes = new Uint8Array(buffer); let text = '';
  for (let i = 0; i < bytes.length; i += 8192) text += String.fromCharCode(...bytes.subarray(i, i + 8192));
  return btoa(text);
}
