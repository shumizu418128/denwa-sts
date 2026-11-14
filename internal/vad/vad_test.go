package vad

import (
	"testing"

	"denwa-sts/internal/config"
)

func makeFrame(amp int16) []int16 {
	frame := make([]int16, config.FrameSamples)
	for i := range frame {
		if i%2 == 0 {
			frame[i] = amp
		} else {
			frame[i] = -amp
		}
	}
	return frame
}

func TestDetectorCapturesSpeechWithPreRoll(t *testing.T) {
	var got [][]int16
	det := New(Options{
		PreRollFrames:    2,
		StartFrames:      2,
		EndSilenceFrames: 4,
		MinSpeechFrames:  3,
	}, func(pcm []int16) {
		got = append(got, pcm)
	})

	// 無音 3 フレーム
	for i := 0; i < 3; i++ {
		det.PushPCM(makeFrame(10))
	}
	// 有音 5 フレーム
	for i := 0; i < 5; i++ {
		det.PushPCM(makeFrame(2000))
	}
	// 無音 4 フレームで確定
	for i := 0; i < 4; i++ {
		det.PushPCM(makeFrame(10))
	}

	if len(got) != 1 {
		t.Fatalf("utterances=%d", len(got))
	}
	// preRoll(2) + speech(5) + silence(4) = 11 frames
	wantSamples := 11 * config.FrameSamples
	if len(got[0]) != wantSamples {
		t.Fatalf("samples=%d want=%d", len(got[0]), wantSamples)
	}
}

func TestDetectorIgnoresShortNoiseBurst(t *testing.T) {
	var got [][]int16
	det := New(Options{
		StartFrames:      2,
		EndSilenceFrames: 3,
		MinSpeechFrames:  5,
	}, func(pcm []int16) {
		got = append(got, pcm)
	})

	// 2 フレームだけの短い破裂音
	det.PushPCM(makeFrame(3000))
	det.PushPCM(makeFrame(3000))
	for i := 0; i < 5; i++ {
		det.PushPCM(makeFrame(0))
	}
	if len(got) != 0 {
		t.Fatalf("ノイズを発話と判定した: %d", len(got))
	}
}

func TestDetectorSplitsAtMaxFrames(t *testing.T) {
	var got [][]int16
	det := New(Options{
		PreRollFrames:      1,
		StartFrames:        2,
		EndSilenceFrames:   10,
		MinSpeechFrames:    2,
		MaxUtteranceFrames: 5,
	}, func(pcm []int16) {
		got = append(got, pcm)
	})

	det.PushPCM(makeFrame(0)) // preRoll
	for i := 0; i < 5; i++ {
		det.PushPCM(makeFrame(2000))
	}
	if len(got) != 1 {
		t.Fatalf("utterances=%d", len(got))
	}
}
