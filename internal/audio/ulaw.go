package audio

import (
	"math"

	"denwa-sts/internal/config"
)

const (
	ulawBias = 0x84
	ulawClip = 32635
)

var ulawDecodeTable = buildUlawDecodeTable()

func buildUlawDecodeTable() [256]int16 {
	var table [256]int16
	for i := range table {
		u := ^byte(i)
		t := (int(u&0x0F) << 3) + ulawBias
		t <<= (u & 0x70) >> 4
		if u&0x80 != 0 {
			table[i] = int16(ulawBias - t)
		} else {
			table[i] = int16(t - ulawBias)
		}
	}
	return table
}

func linearToUlaw(sample int16) byte {
	value := int(sample)
	sign := 0
	if value < 0 {
		value = -value
		sign = 0x80
	}
	if value > ulawClip {
		value = ulawClip
	}
	value += ulawBias

	exponent := 7
	for mask := 0x4000; value&mask == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0F
	return ^byte(sign | exponent<<4 | mantissa)
}

// DecodeULaw は 8 kHz μ-law バイト列を PCM16 サンプルにする。
func DecodeULaw(ulaw []byte) []int16 {
	out := make([]int16, len(ulaw))
	for i, b := range ulaw {
		out[i] = ulawDecodeTable[b]
	}
	return out
}

// EncodeULaw は PCM16 サンプル列を μ-law バイト列にする。
func EncodeULaw(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, sample := range pcm {
		out[i] = linearToUlaw(sample)
	}
	return out
}

// PCM16SamplesToULaw は rate Hz のモノラル PCM16 を 8 kHz μ-law に変換する。
// 8 kHz の整数倍からのダウンサンプリングでは区間平均を取って折り返しを抑える。
func PCM16SamplesToULaw(pcm []int16, rate int) ([]byte, error) {
	if len(pcm) == 0 || rate <= 0 {
		return nil, nil
	}
	target := config.SampleRate
	var down []int16
	if rate == target {
		down = pcm
	} else if rate%target == 0 {
		ratio := rate / target
		down = make([]int16, 0, len(pcm)/ratio+1)
		for start := 0; start < len(pcm); start += ratio {
			end := min(start+ratio, len(pcm))
			sum := 0
			for _, s := range pcm[start:end] {
				sum += int(s)
			}
			down = append(down, int16(sum/(end-start)))
		}
	} else {
		var err error
		down, err = Resample(pcm, rate, target)
		if err != nil {
			return nil, err
		}
	}
	return EncodeULaw(down), nil
}

// StreamToULaw は複数チャンクに分かれた PCM16 を連続して 8 kHz μ-law に変換する。
type StreamToULaw struct {
	pending []int16
}

// Push は rate Hz の PCM16 チャンクを 8 kHz μ-law に変換して返す。
// 整数倍レートの端数サンプルは次の Push または Flush まで保持する。
func (s *StreamToULaw) Push(pcm []int16, rate int) ([]byte, error) {
	if len(pcm) == 0 || rate <= 0 {
		return nil, nil
	}
	target := config.SampleRate
	if rate%target != 0 {
		return PCM16SamplesToULaw(pcm, rate)
	}
	if rate == target {
		return EncodeULaw(pcm), nil
	}

	ratio := rate / target
	all := make([]int16, 0, len(s.pending)+len(pcm))
	all = append(all, s.pending...)
	all = append(all, pcm...)

	usable := (len(all) / ratio) * ratio
	s.pending = append(s.pending[:0], all[usable:]...)
	if usable == 0 {
		return nil, nil
	}

	down := make([]int16, 0, usable/ratio)
	for start := 0; start < usable; start += ratio {
		sum := 0
		for _, sample := range all[start : start+ratio] {
			sum += int(sample)
		}
		down = append(down, int16(sum/ratio))
	}
	return EncodeULaw(down), nil
}

// Flush は保持している端数サンプルを μ-law に変換して返す。
func (s *StreamToULaw) Flush() []byte {
	if len(s.pending) == 0 {
		return nil
	}
	sum := 0
	for _, sample := range s.pending {
		sum += int(sample)
	}
	out := EncodeULaw([]int16{int16(sum / len(s.pending))})
	s.pending = s.pending[:0]
	return out
}

// WavToUlaw は PCM16 モノラル WAV バイト列を 8 kHz μ-law に変換する。
func WavToUlaw(wav []byte) ([]byte, error) {
	pcm, rate, err := DecodeWAV(wav)
	if err != nil {
		return nil, err
	}
	return PCM16SamplesToULaw(pcm, rate)
}

// ThinkingToneFrame は処理待ち中に流す「プップッ」音（1.2秒周期）の 20ms フレームを返す。
// 0〜60ms と 120〜180ms に 440Hz + 880Hz の柔らかいトーンを鳴らし、残りは無音にする。
func ThinkingToneFrame(frameIndex int) []byte {
	const (
		cycleFrames = 60 // 1.2s (20ms * 60)
		pipFrames   = 3  // 60ms
		gapFrames   = 3  // 60ms
	)
	frame := make([]byte, config.FrameSamples)
	pos := frameIndex % cycleFrames
	var localFrame int
	switch {
	case pos < pipFrames:
		localFrame = pos
	case pos >= pipFrames+gapFrames && pos < 2*pipFrames+gapFrames:
		localFrame = pos - (pipFrames + gapFrames)
	default:
		for i := range frame {
			frame[i] = config.ULawSilence
		}
		return frame
	}

	totalSamples := pipFrames * config.FrameSamples
	baseSample := localFrame * config.FrameSamples
	fadeSamples := config.FrameSamples / 2 // 10ms フェード
	for i := range frame {
		sampleIdx := baseSample + i
		env := 1.0
		if sampleIdx < fadeSamples {
			env = float64(sampleIdx) / float64(fadeSamples)
		} else if rem := totalSamples - 1 - sampleIdx; rem < fadeSamples {
			env = float64(rem) / float64(fadeSamples)
		}
		t := float64(sampleIdx) / float64(config.SampleRate)
		val := env * (1800*math.Sin(2*math.Pi*440*t) + 900*math.Sin(2*math.Pi*880*t))
		frame[i] = linearToUlaw(clampInt16(val))
	}
	return frame
}




