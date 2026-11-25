class Recorder extends AudioWorkletProcessor {
  constructor() { super(); this.buffer = new Float32Array(1024); this.offset = 0; }
  process(inputs) {
    const samples = inputs[0]?.[0];
    if (samples) for (const value of samples) {
      this.buffer[this.offset++] = value;
      if (this.offset === this.buffer.length) {
        this.port.postMessage(this.buffer, [this.buffer.buffer]);
        this.buffer = new Float32Array(1024); this.offset = 0;
      }
    }
    // 스피커 출력은 비워 두고 다음 마이크 입력도 계속 받는다.
    return true;
  }
}
registerProcessor('voice-recorder', Recorder);
