package pcm

import (
	"encoding/binary"
	"math"
	"strconv"
	"testing"
)

func TestNormalizeCanonicalAndResample(t *testing.T) {
	for _, rate := range []int{16000, 22050, 48000} {
		t.Run(strconv.Itoa(rate), func(t *testing.T) {
			samples := make([]float32, rate)
			for i := range samples {
				samples[i] = float32(math.Sin(float64(i)*2*math.Pi*440/float64(rate))) * .2
			}
			wav := Encode(samples)
			binary.LittleEndian.PutUint32(wav[24:], uint32(rate))
			binary.LittleEndian.PutUint32(wav[28:], uint32(rate*2))
			actual, e := Normalize(wav)
			if e != nil || len(actual) != 32044 {
				t.Fatal(len(actual), e)
			}
			if binary.LittleEndian.Uint32(actual[24:]) != 16000 {
				t.Fatal("Wrong normalized rate")
			}
		})
	}
}
func TestMalformedWAVDoesNotReachSampleConversion(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("RIFF"), make([]byte, 44), Encode(make([]float32, 100))} {
		if _, e := Normalize(data); e == nil {
			t.Fatal("Malformed or undersized WAV accepted")
		}
	}
}
