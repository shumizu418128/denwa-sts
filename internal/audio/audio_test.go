package audio

import (
	"bytes"
	"math"
	"path/filepath"
	"testing"
)

func TestWAVRoundTrip(t *testing.T) {
	pcm := []int16{0, 1000, -1000, 32767, -32768}
	path := filepath.Join(t.TempDir(), "tone.wav")
	if err := WriteWAV(path, pcm, 16000); err != nil {
		t.Fatal(err)
	}
	got, rate, err := ReadWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 16000 {
		t.Fatalf("rate=%d", rate)
	}
	if len(got) != len(pcm) {
		t.Fatalf("len=%d", len(got))
	}
	for i := range pcm {
		if got[i] != pcm[i] {
			t.Fatalf("sample[%d]=%d", i, got[i])
		}
	}
}

func TestEvenBytesDropsTrailingByte(t *testing.T) {
	got := EvenBytes([]byte{1, 2, 3})
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got=%v", got)
	}
	pcm, err := PCM16FromBytes(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) != 1 {
		t.Fatalf("samples=%d", len(pcm))
	}
}

func TestPCM16BytesAreEven(t *testing.T) {
	encoded := PCM16Bytes([]int16{1, -2, 3})
	if len(encoded)%2 != 0 {
		t.Fatalf("len=%d", len(encoded))
	}
}

func TestResampleDoublesLength(t *testing.T) {
	in := []int16{0, 100}
	got, err := Resample(in, 8000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	// n = round(2 * 16000 / 8000) = 4。位置は 0, 0.5, 1, 1.5。
	want := []int16{0, 50, 100, 100}
	if len(got) != len(want) {
		t.Fatalf("len=%d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample[%d]=%d", i, got[i])
		}
	}
}

func TestResampleSameRateCopies(t *testing.T) {
	in := []int16{1, 2, 3}
	got, err := Resample(in, 16000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	in[0] = 9
	if got[0] != 1 {
		t.Fatalf("copy=%d", got[0])
	}
}

func TestDecodeWAVRejectsStereo(t *testing.T) {
	mono := EncodeWAV([]int16{1}, 8000)
	stereo := bytes.Clone(mono)
	// fmt のチャンネル数はオフセット 22。
	stereo[22] = 2
	if _, _, err := DecodeWAV(stereo); err == nil {
		t.Fatal("ステレオを受けた")
	}
}

func TestUlawRoundTrip(t *testing.T) {
	for _, sample := range []int16{0, 100, -100, 1000, -1000, 8000, -8000, 30000, -30000} {
		decoded := DecodeULaw(EncodeULaw([]int16{sample}))[0]
		tolerance := math.Max(16, math.Abs(float64(sample))*0.07)
		if diff := math.Abs(float64(decoded) - float64(sample)); diff > tolerance {
			t.Errorf("sample=%d decoded=%d diff=%.0f", sample, decoded, diff)
		}
	}
}

func TestPCM16SamplesToULaw24kTo8k(t *testing.T) {
	pcm := make([]int16, 2400) // 100ms @ 24kHz -> 800 bytes @ 8kHz
	ulaw, err := PCM16SamplesToULaw(pcm, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ulaw) != 800 {
		t.Fatalf("len=%d", len(ulaw))
	}
	for i, b := range ulaw {
		if b != 0xFF {
			t.Fatalf("ulaw[%d]=%#x", i, b)
		}
	}
}

func TestStreamToULawCarriesRemainder(t *testing.T) {
	var conv StreamToULaw
	out1, err := conv.Push([]int16{100, 100, 100, 200}, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out1) != 1 {
		t.Fatalf("out1 len=%d", len(out1))
	}
	out2, err := conv.Push([]int16{200, 200, 300}, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 1 {
		t.Fatalf("out2 len=%d", len(out2))
	}
	out3 := conv.Flush()
	if len(out3) != 1 {
		t.Fatalf("out3 len=%d", len(out3))
	}
}


