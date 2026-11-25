// 사용자가 재생을 멈춘 뒤 이전 합성 요청이 완료될 수 있다.
// 요청 순서대로 합성하되 재생 직전에도 중단 이후의 요청인지 확인한다.
export class SpeechQueue {
  constructor({ synthesize, play, error = () => {} }) {
    Object.assign(this, { synthesize, play, error });
    this.generation = 0; this.pending = []; this.running = false; this.controller = null; this.closed = false;
  }
  enqueue(effect) {
    if (this.closed || effect.generation !== this.generation) return;
    if (this.pending.length >= 8) return this.error(new Error('일본어 음성 대기열이 가득 찼습니다. 화면을 확인하고 통역을 다시 시작해 주세요.'));
    this.pending.push(effect); this.drain();
  }
  async drain() {
    if (this.running) return; this.running = true;
    while (!this.closed && this.pending.length) {
      const effect = this.pending.shift();
      if (effect.generation !== this.generation) continue;
      const controller = this.controller = new AbortController();
      try {
        const audio = await this.synthesize(effect.text, controller.signal);
        if (this.closed || controller.signal.aborted || effect.generation !== this.generation) continue;
        await this.play(audio, controller.signal, effect);
      } catch (error) { if (!controller.signal.aborted && !this.closed) this.error(error); }
      finally { if (this.controller === controller) this.controller = null; }
    }
    this.running = false;
  }
  clear(nextGeneration = this.generation + 1) { this.generation = nextGeneration; this.pending = []; this.controller?.abort(); }
  close() { this.closed = true; this.clear(); }
}
