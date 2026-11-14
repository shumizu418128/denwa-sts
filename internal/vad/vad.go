// Package vad は電話音声 (8 kHz PCM16) の発話区間をエネルギーベースで検出する。
package vad

import (
	"math"

	"denwa-sts/internal/config"
)

const (
	defaultPreRollFrames      = 15  // 300 ms
	defaultStartFrames        = 2   // 40 ms
	defaultBargeInFrames      = 4   // 80 ms
	defaultEndSilenceFrames   = 40  // 800 ms
	defaultMinSpeechFrames    = 8   // 160 ms
	defaultMaxUtteranceFrames = 750 // 15 s
	defaultMinStartRMS        = 180.0
	defaultMaxStartRMS        = 1800.0
	defaultNoiseMultiplier    = 2.8
	defaultEndRatio           = 0.65
	initialNoiseRMS           = 40.0
)

// Options は VAD の調整パラメータ。ゼロ値の項目は既定値を使う。
type Options struct {
	PreRollFrames      int
	StartFrames        int
	BargeInFrames      int
	EndSilenceFrames   int
	MinSpeechFrames    int
	MaxUtteranceFrames int
	MinStartRMS        float64
	MaxStartRMS        float64
	NoiseMultiplier    float64
	EndRatio           float64
	// OnSpeechStart は発話中の有音フレームが BargeInFrames に達した時点で1回呼ばれる。
	OnSpeechStart func()
}

func (o Options) withDefaults() Options {
	if o.PreRollFrames <= 0 {
		o.PreRollFrames = defaultPreRollFrames
	}
	if o.StartFrames <= 0 {
		o.StartFrames = defaultStartFrames
	}
	if o.BargeInFrames <= 0 {
		o.BargeInFrames = defaultBargeInFrames
	}
	if o.EndSilenceFrames <= 0 {
		o.EndSilenceFrames = defaultEndSilenceFrames
	}
	if o.MinSpeechFrames <= 0 {
		o.MinSpeechFrames = defaultMinSpeechFrames
	}
	if o.MaxUtteranceFrames <= 0 {
		o.MaxUtteranceFrames = defaultMaxUtteranceFrames
	}
	if o.MinStartRMS <= 0 {
		o.MinStartRMS = defaultMinStartRMS
	}
	if o.MaxStartRMS <= 0 {
		o.MaxStartRMS = defaultMaxStartRMS
	}
	if o.NoiseMultiplier <= 0 {
		o.NoiseMultiplier = defaultNoiseMultiplier
	}
	if o.EndRatio <= 0 {
		o.EndRatio = defaultEndRatio
	}
	return o
}

// Detector は 8 kHz PCM16 の連続入力から 1 発話を切り出す。
// 1つの goroutine から呼ぶこと。
type Detector struct {
	opts        Options
	onUtterance func(pcm []int16)

	noiseRMS        float64
	preRoll         [][]int16
	pendingStart    [][]int16
	speaking        bool
	bargeInNotified bool
	utterance       []int16
	totalFrames     int
	speechFrames    int
	silenceFrames   int
}

// New は Detector を作る。発話が確定すると onUtterance に 8 kHz PCM16 を渡す。
func New(opts Options, onUtterance func(pcm []int16)) *Detector {
	return &Detector{
		opts:        opts.withDefaults(),
		onUtterance: onUtterance,
		noiseRMS:    initialNoiseRMS,
	}
}

// PushPCM は 8 kHz PCM16 のサンプル列（通常 160 サンプル = 20ms）を入力する。
func (d *Detector) PushPCM(samples []int16) {
	if len(samples) == 0 {
		return
	}
	frame := make([]int16, len(samples))
	copy(frame, samples)

	rms := frameRMS(frame)
	startThreshold := d.startThreshold()
	endThreshold := startThreshold * d.opts.EndRatio

	if !d.speaking {
		if rms >= startThreshold {
			d.pendingStart = append(d.pendingStart, frame)
			if len(d.pendingStart) >= d.opts.StartFrames {
				d.beginUtterance()
			}
			return
		}
		// 一瞬のスパイクはプリロールへ逃がしつつノイズフロアを更新する
		if len(d.pendingStart) > 0 {
			for _, f := range d.pendingStart {
				d.pushPreRoll(f)
			}
			d.pendingStart = nil
		}
		d.updateNoiseFloor(rms)
		d.pushPreRoll(frame)
		return
	}

	d.utterance = append(d.utterance, frame...)
	d.totalFrames++
	if rms >= endThreshold {
		d.speechFrames++
		d.silenceFrames = 0
		if !d.bargeInNotified && d.speechFrames >= d.opts.BargeInFrames {
			d.bargeInNotified = true
			if d.opts.OnSpeechStart != nil {
				d.opts.OnSpeechStart()
			}
		}
	} else {
		d.silenceFrames++
	}

	if d.silenceFrames >= d.opts.EndSilenceFrames {
		// 末尾の長い無音を少しだけ残して切る
		keepSilence := min(d.silenceFrames, 10)
		trimSamples := (d.silenceFrames - keepSilence) * config.FrameSamples
		if trimSamples > 0 && trimSamples < len(d.utterance) {
			d.utterance = d.utterance[:len(d.utterance)-trimSamples]
		}
		d.finishUtterance()
		return
	}
	if d.totalFrames >= d.opts.MaxUtteranceFrames {
		d.finishUtterance()
	}
}

// Flush は現在録音中の発話があれば確定して出力する。なければ状態をクリアする。
func (d *Detector) Flush() {
	if d.speaking {
		d.finishUtterance()
		return
	}
	d.Reset()
}

// Reset は録音中の状態とプリロールを破棄する。ノイズ推定は維持する。
func (d *Detector) Reset() {
	d.preRoll = nil
	d.pendingStart = nil
	d.speaking = false
	d.bargeInNotified = false
	d.utterance = nil
	d.totalFrames = 0
	d.speechFrames = 0
	d.silenceFrames = 0
}

func (d *Detector) startThreshold() float64 {
	th := d.noiseRMS * d.opts.NoiseMultiplier
	if th < d.opts.MinStartRMS {
		return d.opts.MinStartRMS
	}
	if th > d.opts.MaxStartRMS {
		return d.opts.MaxStartRMS
	}
	return th
}

func (d *Detector) updateNoiseFloor(rms float64) {
	// ゆっくり追従させる（急な突発音で跳ねないように）
	if rms > d.noiseRMS*2 {
		d.noiseRMS = d.noiseRMS*0.98 + rms*0.02
	} else {
		d.noiseRMS = d.noiseRMS*0.90 + rms*0.10
	}
}

func (d *Detector) pushPreRoll(frame []int16) {
	d.preRoll = append(d.preRoll, frame)
	if len(d.preRoll) > d.opts.PreRollFrames {
		d.preRoll = d.preRoll[len(d.preRoll)-d.opts.PreRollFrames:]
	}
}

func (d *Detector) beginUtterance() {
	d.speaking = true
	d.bargeInNotified = false
	d.utterance = make([]int16, 0, (len(d.preRoll)+len(d.pendingStart)+50)*config.FrameSamples)
	for _, f := range d.preRoll {
		d.utterance = append(d.utterance, f...)
	}
	for _, f := range d.pendingStart {
		d.utterance = append(d.utterance, f...)
	}
	d.totalFrames = len(d.pendingStart)
	d.speechFrames = len(d.pendingStart)
	d.silenceFrames = 0
	d.preRoll = nil
	d.pendingStart = nil
	if d.speechFrames >= d.opts.BargeInFrames {
		d.bargeInNotified = true
		if d.opts.OnSpeechStart != nil {
			d.opts.OnSpeechStart()
		}
	}
}

func (d *Detector) finishUtterance() {
	pcm := d.utterance
	speechFrames := d.speechFrames
	d.Reset()
	if speechFrames >= d.opts.MinSpeechFrames && len(pcm) > 0 && d.onUtterance != nil {
		d.onUtterance(pcm)
	}
}

// NoiseRMS は現在の推定ノイズレベルを返す。
func (d *Detector) NoiseRMS() float64 { return d.noiseRMS }

// StartThreshold は現在の発話開始しきい値 (RMS) を返す。
func (d *Detector) StartThreshold() float64 { return d.startThreshold() }

// Speaking は現在発話区間として録音中かどうかを返す。
func (d *Detector) Speaking() bool { return d.speaking }

// FrameStats は 160 サンプル等の PCM16 フレームの RMS と最大絶対振幅を返す。
func FrameStats(samples []int16) (rms float64, peak int16) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	var maxAbs int32
	for _, s := range samples {
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
		f := float64(s)
		sum += f * f
	}
	if maxAbs > math.MaxInt16 {
		maxAbs = math.MaxInt16
	}
	return math.Sqrt(sum / float64(len(samples))), int16(maxAbs)
}

func frameRMS(samples []int16) float64 {
	var sum float64
	for _, s := range samples {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}
