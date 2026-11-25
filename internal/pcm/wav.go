package pcm

import (
	"encoding/binary"
	"errors"
	"math"
)

// Normalize는 크기 제한 안의 PCM16 모노 WAV를 브라우저 규격에 맞는 16 kHz WAV로 변환한다.
// 부가 RIFF 청크가 있는 파일도 읽는다. 파일로 음성을 점검할 때 사용한다.
func Normalize(wav []byte) ([]byte, error) {
	bad := errors.New("Use a PCM16 mono WAV up to 20 seconds")
	if len(wav) < 44 || len(wav) > 4*1024*1024 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, bad
	}
	var samples []byte
	rate := 0
	for offset := 12; offset+8 <= len(wav); {
		n := int(binary.LittleEndian.Uint32(wav[offset+4:]))
		start := offset + 8
		if n < 0 || n > len(wav)-start {
			return nil, bad
		}
		switch string(wav[offset : offset+4]) {
		case "fmt ":
			if n < 16 || binary.LittleEndian.Uint16(wav[start:]) != 1 || binary.LittleEndian.Uint16(wav[start+2:]) != 1 || binary.LittleEndian.Uint16(wav[start+14:]) != 16 {
				return nil, bad
			}
			rate = int(binary.LittleEndian.Uint32(wav[start+4:]))
		case "data":
			samples = wav[start : start+n]
		}
		offset = start + n + n%2
	}
	if rate < 8000 || rate > 96000 || len(samples)%2 != 0 || len(samples)/2 < rate*3/10 || len(samples)/2 > rate*20 {
		return nil, bad
	}
	count := len(samples) / 2 * 16000 / rate
	out := make([]float32, count)
	for i := range out {
		position := float64(i) * float64(rate) / 16000
		index := int(position)
		fraction := position - float64(index)
		a := int16(binary.LittleEndian.Uint16(samples[index*2:]))
		next := index + 1
		if next >= len(samples)/2 {
			next = index
		}
		b := int16(binary.LittleEndian.Uint16(samples[next*2:]))
		out[i] = float32(float64(a)*(1-fraction)+float64(b)*fraction) / 32768
	}
	return Encode(out), nil
}
func Encode(samples []float32) []byte {
	wav := make([]byte, 44+len(samples)*2)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(samples)*2))
	for i, v := range samples {
		sample := math.Round(float64(v) * 32768)
		sample = math.Min(32767, math.Max(-32768, sample))
		binary.LittleEndian.PutUint16(wav[44+i*2:], uint16(int16(sample)))
	}
	return wav
}
