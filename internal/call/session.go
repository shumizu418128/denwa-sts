// Package call は 1 通話分の処理を行う。応答 → ブリッジ → 発話検出 → STS 応答 → 音声送出。
package call

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"denwa-sts/internal/ari"
	"denwa-sts/internal/audio"
	"denwa-sts/internal/config"
	"denwa-sts/internal/rtp"
	"denwa-sts/internal/vad"
)

const (
	mediaReadyTimeout = 2 * time.Second
	cleanupTimeout    = 10 * time.Second
	dialRetryDelay    = time.Second
	resumeCooldown    = 200 * time.Millisecond
	rxQueueSize       = 512
	utteranceQueue    = 4
	streamChanSize    = 64
	textChunkQueue    = 8
	// toneLeadFrames は保留音を、RTP の 20ms 間隔より少し先まで溜める量。
	toneLeadFrames = 3
)

var unusablePeerAddrs = map[string]bool{"": true, "0.0.0.0": true, "127.0.0.1": true, "::": true}

// ARI は通話の制御に使う ARI の操作。
type ARI interface {
	Answer(ctx context.Context, channelID string) error
	Hangup(ctx context.Context, channelID string)
	ChannelVar(ctx context.Context, channelID, variable string) (string, error)
	CreateExternalMedia(ctx context.Context, externalHost, channelID string) (*ari.Channel, error)
	CreateBridge(ctx context.Context) (string, error)
	AddToBridge(ctx context.Context, bridgeID, channelID string) error
	DestroyBridge(ctx context.Context, bridgeID string)
}

// Responder は 1 発話の音声を受け取り、返答テキストを返す。
type Responder interface {
	RespondText(ctx context.Context, pcm []int16, sampleRate int) (string, error)
	Close()
}

// Synthesizer は文字列を電話用の 8 kHz μ-law 音声に変える。
type Synthesizer interface {
	SynthesizeULaw(text string) ([]byte, error)
}

// TextStreamer は返答テキストを、読み上げできる断片ができ次第 onChunk へ渡す。
// 全文の生成完了より前に onChunk が呼ばれる。戻り値は返答全文。
type TextStreamer interface {
	StreamText(ctx context.Context, pcm []int16, sampleRate int, onChunk func(chunk string) error) (string, error)
}

// Options は通話ごとのセッションを作るための部品と設定。
type Options struct {
	ARI           ARI
	DialResponder func(ctx context.Context) (Responder, error)
	Synth         Synthesizer
	// RTPBindHost は RTP を受ける UDP ソケットを開くアドレス。
	RTPBindHost string
	// FallbackPeerHost は ExternalMedia が使えないアドレスを返したときの RTP 送信先。
	FallbackPeerHost string
	VAD              vad.Options
}

// Session は 1 通話分の状態。
type Session struct {
	opts      Options
	channelID string
	callerNum string
	mediaID   string

	rtp *rtp.Session
	vad *vad.Detector

	ctx    context.Context
	cancel context.CancelFunc

	bridgeMu sync.Mutex
	bridgeID string

	responderMu sync.Mutex
	responder   Responder

	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	rxQueue     chan []byte
	utteranceCh chan []int16

	mediaReady     chan struct{}
	mediaReadyOnce sync.Once
	cleanupOnce    sync.Once
}

// NewSession は RTP 用のソケットと発話検出器を用意する。通話の処理は Start で始める。
func NewSession(opts Options, channelID, callerNum string) (*Session, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		opts:        opts,
		channelID:   channelID,
		callerNum:   callerNum,
		mediaID:     "extmedia-" + channelID,
		ctx:         ctx,
		cancel:      cancel,
		rxQueue:     make(chan []byte, rxQueueSize),
		utteranceCh: make(chan []int16, utteranceQueue),
		mediaReady:  make(chan struct{}),
	}
	vadOpts := opts.VAD
	userOnSpeechStart := vadOpts.OnSpeechStart
	vadOpts.OnSpeechStart = func() {
		if userOnSpeechStart != nil {
			userOnSpeechStart()
		}
		s.onCallerSpeechStart()
	}
	s.vad = vad.New(vadOpts, s.enqueueUtterance)

	var err error
	s.rtp, err = rtp.NewSession(opts.RTPBindHost, s.onRTPULaw)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("RTPソケット作成失敗: %w", err)
	}
	return s, nil
}

// ChannelID は着信チャネルの ID。
func (s *Session) ChannelID() string { return s.channelID }

// MediaID は ExternalMedia チャネルの ID。
func (s *Session) MediaID() string { return s.mediaID }

// Start は通話の処理を始める。
func (s *Session) Start() {
	slog.Info("着信", "channel", s.channelID, "caller", s.callerNum)
	go s.rxLoop()
	go s.respondLoop()
	s.rtp.Start()
	go s.run()
}

// NotifyMediaReady は ExternalMedia チャネルが Stasis に入ったことを知らせる。
func (s *Session) NotifyMediaReady() {
	s.mediaReadyOnce.Do(func() { close(s.mediaReady) })
}

// Cleanup は通話の後始末をする。何度呼んでもよい。
func (s *Session) Cleanup() {
	s.cleanupOnce.Do(func() {
		s.cancel()
		s.rtp.Stop()
		s.closeResponder()

		// s.ctx は取り消し済みなので、後始末の ARI 呼び出しは別の ctx で行う
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		s.opts.ARI.Hangup(ctx, s.mediaID)
		if bridgeID := s.takeBridgeID(); bridgeID != "" {
			s.opts.ARI.DestroyBridge(ctx, bridgeID)
		}
	})
}

func (s *Session) takeBridgeID() string {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	id := s.bridgeID
	s.bridgeID = ""
	return id
}

// --- 音声受信と VAD ---

func (s *Session) onRTPULaw(payload []byte) {
	if len(payload) == 0 || s.ctx.Err() != nil {
		return
	}
	select {
	case s.rxQueue <- payload:
	default:
		slog.Warn("受信音声キューが満杯のため捨てた", "channel", s.channelID)
	}
}

func (s *Session) rxLoop() {
	var frames int
	var sumRMS float64
	var maxRMS float64
	var maxPeak int16
	var nonZero int
	lastReport := time.Now()

	report := func() {
		if frames == 0 {
			return
		}
		slog.Debug("受信音声レベル",
			"channel", s.channelID,
			"frames", frames,
			"nonzero_frames", nonZero,
			"avg_rms", int(sumRMS/float64(frames)),
			"max_rms", int(maxRMS),
			"peak", maxPeak,
			"noise_rms", int(s.vad.NoiseRMS()),
			"threshold", int(s.vad.StartThreshold()),
			"speaking", s.vad.Speaking(),
		)
		frames = 0
		sumRMS = 0
		maxRMS = 0
		maxPeak = 0
		nonZero = 0
		lastReport = time.Now()
	}
	defer report()

	for {
		select {
		case <-s.ctx.Done():
			return
		case payload := <-s.rxQueue:
			pcm := audio.DecodeULaw(payload)
			rms, peak := vad.FrameStats(pcm)
			frames++
			sumRMS += rms
			if rms > maxRMS {
				maxRMS = rms
			}
			if peak > maxPeak {
				maxPeak = peak
			}
			if peak > 0 {
				nonZero++
			}
			if time.Since(lastReport) >= 5*time.Second {
				report()
			}
			s.vad.PushPCM(pcm)
		}
	}
}

func (s *Session) onCallerSpeechStart() {
	if s.interruptTurn() {
		slog.Info("割り込み発話検出により応答中断", "channel", s.channelID)
	}
}

func (s *Session) interruptTurn() bool {
	s.turnMu.Lock()
	cancel := s.turnCancel
	s.turnCancel = nil
	s.turnMu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Session) enqueueUtterance(pcm []int16) {
	if len(pcm) == 0 || s.ctx.Err() != nil {
		return
	}
	s.interruptTurn()
	dur := time.Duration(len(pcm)) * time.Second / config.SampleRate
	slog.Info("発話検出", "channel", s.channelID, "samples", len(pcm), "duration", dur.Round(10*time.Millisecond))
	// 古い未処理発話があれば捨てて最新の発話を優先する
	for {
		select {
		case <-s.utteranceCh:
		default:
			select {
			case s.utteranceCh <- pcm:
			default:
			}
			return
		}
	}
}

// --- STS 応答ループ ---

func (s *Session) respondLoop() {
	// 最初の発話が来る前にモデル接続を温めておく
	if _, err := s.ensureResponder(); err != nil && s.ctx.Err() == nil {
		slog.Warn("STS事前接続失敗", "channel", s.channelID, "error", err)
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case pcm := <-s.utteranceCh:
			s.respondOne(pcm)
		}
	}
}

func (s *Session) ensureResponder() (Responder, error) {
	s.responderMu.Lock()
	if s.responder != nil {
		res := s.responder
		s.responderMu.Unlock()
		return res, nil
	}
	s.responderMu.Unlock()

	if s.opts.DialResponder == nil {
		return nil, fmt.Errorf("DialResponder が未設定です")
	}
	res, err := s.opts.DialResponder(s.ctx)
	if err != nil {
		return nil, err
	}

	s.responderMu.Lock()
	if s.ctx.Err() != nil {
		s.responderMu.Unlock()
		res.Close()
		return nil, s.ctx.Err()
	}
	if s.responder != nil {
		s.responderMu.Unlock()
		res.Close()
		return s.responder, nil
	}
	s.responder = res
	s.responderMu.Unlock()
	slog.Info("STSセッション接続完了", "channel", s.channelID)
	return res, nil
}

func (s *Session) closeResponder() {
	s.responderMu.Lock()
	res := s.responder
	s.responder = nil
	s.responderMu.Unlock()
	if res != nil {
		res.Close()
	}
}

func (s *Session) respondOne(pcm []int16) {
	if s.ctx.Err() != nil {
		return
	}

	turnCtx, turnCancel := context.WithCancel(s.ctx)
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnCancel = turnCancel
	s.turnMu.Unlock()
	defer func() {
		s.turnMu.Lock()
		s.turnCancel = nil
		s.turnMu.Unlock()
		turnCancel()
	}()

	res, err := s.ensureResponder()
	if err != nil {
		if turnCtx.Err() == nil {
			slog.Error("STS接続失敗", "channel", s.channelID, "error", err)
			select {
			case <-turnCtx.Done():
			case <-time.After(dialRetryDelay):
			}
		}
		return
	}

	started := time.Now()
	play := s.openPlayback(turnCtx)
	defer play.finish()

	if streamer, ok := res.(TextStreamer); ok {
		s.respondStreaming(turnCtx, streamer, pcm, started, play)
		return
	}
	s.respondBuffered(turnCtx, res, pcm, started, play)
}

// utterancePlayback は保留音と応答音声を同じ RTP 列へ流す。
type utterancePlayback struct {
	ulawCh   chan []byte
	sendDone <-chan bool
	stopTone func()

	finishOnce sync.Once
	sentOK     bool
}

// openPlayback は RTP 送出と、最初の応答音声が来るまでの保留音を始める。
func (s *Session) openPlayback(turnCtx context.Context) *utterancePlayback {
	ulawCh := make(chan []byte, streamChanSize)
	sendDone := make(chan bool, 1)
	go func() {
		sendDone <- s.rtp.SendULawStreamContext(turnCtx, ulawCh)
	}()

	stopTone := make(chan struct{})
	toneDone := make(chan struct{})
	var stopOnce sync.Once
	play := &utterancePlayback{
		ulawCh:   ulawCh,
		sendDone: sendDone,
		stopTone: func() {
			stopOnce.Do(func() { close(stopTone) })
			<-toneDone
		},
	}
	// 最初の応答音声まで「プップッ」という保留音を流す。
	// 数フレーム先まで溜めて、送信側が空いた瞬間に無音を挟まないようにする。
	go func() {
		defer close(toneDone)
		for frameIdx := 0; ; frameIdx++ {
			select {
			case <-turnCtx.Done():
				return
			case <-stopTone:
				return
			case ulawCh <- audio.ThinkingToneFrame(frameIdx):
			}
			if len(ulawCh) < toneLeadFrames {
				continue
			}
			timer := time.NewTimer(config.FrameDuration)
			select {
			case <-turnCtx.Done():
				timer.Stop()
				return
			case <-stopTone:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return play
}

func (p *utterancePlayback) finish() bool {
	p.finishOnce.Do(func() {
		p.stopTone()
		close(p.ulawCh)
		p.sentOK = <-p.sendDone
	})
	return p.sentOK
}

func (s *Session) respondBuffered(turnCtx context.Context, res Responder, pcm []int16, started time.Time, play *utterancePlayback) {
	text, err := res.RespondText(turnCtx, pcm, config.SampleRate)
	if err != nil && turnCtx.Err() == nil {
		s.closeResponder()
		if res2, dialErr := s.ensureResponder(); dialErr == nil {
			text, err = res2.RespondText(turnCtx, pcm, config.SampleRate)
		}
	}
	if err != nil {
		play.finish()
		s.closeResponder()
		if turnCtx.Err() == nil {
			slog.Error("STS応答失敗", "channel", s.channelID, "error", err)
		}
		return
	}
	textElapsed := time.Since(started).Round(10 * time.Millisecond)
	slog.Info("STSテキスト生成完了", "channel", s.channelID, "text", text, "text_elapsed", textElapsed)

	if s.opts.Synth == nil {
		play.finish()
		slog.Error("Synthesizer が未設定です", "channel", s.channelID)
		return
	}
	ulaw, err := s.opts.Synth.SynthesizeULaw(text)
	play.stopTone()
	if err != nil || turnCtx.Err() != nil {
		play.finish()
		if err != nil && turnCtx.Err() == nil {
			slog.Error("VOICEVOX合成失敗", "channel", s.channelID, "text", text, "error", err)
		}
		return
	}
	if len(ulaw) > 0 {
		sendULawChunk(turnCtx, play.ulawCh, leadSilence())
		sendULawChunk(turnCtx, play.ulawCh, ulaw)
	}
	sentOK := play.finish()
	slog.Info("STS応答完了", "channel", s.channelID, "text", text,
		"bytes", len(ulaw), "sent_ok", sentOK, "interrupted", turnCtx.Err() != nil, "elapsed", time.Since(started).Round(10*time.Millisecond))
}

// respondStreaming はテキスト断片が揃い次第、全文の生成完了を待たずに音声合成と送出を始める。
// 次の断片が再生に追いつかないあいだは、RTP 側が無音で送出時刻を保つ。
func (s *Session) respondStreaming(turnCtx context.Context, streamer TextStreamer, pcm []int16, started time.Time, play *utterancePlayback) {
	if s.opts.Synth == nil {
		play.finish()
		slog.Error("Synthesizer が未設定です", "channel", s.channelID)
		return
	}

	streamCtx, streamCancel := context.WithCancel(turnCtx)
	defer streamCancel()

	chunks := make(chan string, textChunkQueue)
	type synthResult struct {
		err       error
		bytes     int
		abortText bool
	}
	synthCh := make(chan synthResult, 1)
	textDone := make(chan struct{})

	go func() {
		var total int
		var pending []byte
		released := false
		var result synthResult

		flush := func() bool {
			if len(pending) == 0 {
				return true
			}
			if !released {
				released = true
				play.stopTone()
				if !sendULawChunk(turnCtx, play.ulawCh, leadSilence()) {
					result.err = turnCtx.Err()
					return false
				}
			}
			if !sendULawChunk(turnCtx, play.ulawCh, pending) {
				result.err = turnCtx.Err()
				return false
			}
			total += len(pending)
			pending = nil
			return true
		}

		for chunk := range chunks {
			if streamCtx.Err() != nil {
				result.err = streamCtx.Err()
				break
			}
			ulaw, err := s.opts.Synth.SynthesizeULaw(chunk)
			if err != nil {
				result.err = err
				select {
				case <-textDone:
				default:
					result.abortText = true
					streamCancel()
				}
				break
			}
			if len(ulaw) == 0 {
				continue
			}
			pending = append(pending, ulaw...)
			slog.Info("STS音声断片", "channel", s.channelID, "text", chunk, "bytes", len(ulaw), "elapsed", time.Since(started).Round(10*time.Millisecond))
			if !flush() {
				break
			}
		}
		if len(pending) > 0 && !flush() && result.err == nil {
			result.err = turnCtx.Err()
		}
		if !released {
			play.stopTone()
		}
		result.bytes = total
		synthCh <- result
	}()

	var emitted bool
	onChunk := func(chunk string) error {
		emitted = true
		slog.Info("STSテキスト断片", "channel", s.channelID, "text", chunk, "elapsed", time.Since(started).Round(10*time.Millisecond))
		select {
		case <-streamCtx.Done():
			return streamCtx.Err()
		case chunks <- chunk:
			return nil
		}
	}

	text, err := streamer.StreamText(streamCtx, pcm, config.SampleRate, onChunk)
	if err != nil && !emitted && turnCtx.Err() == nil && streamCtx.Err() == nil {
		s.closeResponder()
		if res2, dialErr := s.ensureResponder(); dialErr == nil {
			if streamer2, ok := res2.(TextStreamer); ok {
				text, err = streamer2.StreamText(streamCtx, pcm, config.SampleRate, onChunk)
			}
		}
	}
	close(textDone)
	close(chunks)
	result := <-synthCh
	sentOK := play.finish()

	if turnCtx.Err() == nil && result.abortText {
		slog.Error("VOICEVOX合成失敗", "channel", s.channelID, "text", text, "error", result.err)
	} else if turnCtx.Err() == nil && err != nil {
		slog.Error("STS応答失敗", "channel", s.channelID, "text", text, "error", err)
	} else if turnCtx.Err() == nil && result.err != nil {
		slog.Error("VOICEVOX合成失敗", "channel", s.channelID, "text", text, "error", result.err)
	}
	if err != nil || result.abortText {
		s.closeResponder()
		return
	}
	if result.err != nil && turnCtx.Err() == nil {
		return
	}
	slog.Info("STS応答完了", "channel", s.channelID, "text", text,
		"bytes", result.bytes, "sent_ok", sentOK, "interrupted", turnCtx.Err() != nil, "elapsed", time.Since(started).Round(10*time.Millisecond))
}

func leadSilence() []byte {
	lead := make([]byte, config.FrameSamples*5)
	for i := range lead {
		lead[i] = config.ULawSilence
	}
	return lead
}

func sendULawChunk(ctx context.Context, dst chan<- []byte, payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case dst <- payload:
		return true
	}
}

// --- 通話の流れ ---

func (s *Session) run() {
	start := time.Now()
	defer func() {
		s.Cleanup()
		slog.Info("終了", "channel", s.channelID,
			"elapsed", time.Since(start).Round(10*time.Millisecond),
			"rx_packets", s.rtp.RecvPackets(),
			"tx_packets", s.rtp.TxPackets())
	}()

	if err := s.setupMedia(); err != nil {
		if s.ctx.Err() == nil {
			slog.Error("通話処理失敗", "channel", s.channelID, "error", err)
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			s.opts.ARI.Hangup(ctx, s.channelID)
		}
		return
	}
	slog.Info("会話待機", "channel", s.channelID)
	<-s.ctx.Done()
}

// setupMedia は応答し、ブリッジと ExternalMedia を作って、媒体が準備できたら ExternalMedia もブリッジへ入れる。
func (s *Session) setupMedia() error {
	pbx := s.opts.ARI
	if err := pbx.Answer(s.ctx, s.channelID); err != nil {
		return err
	}
	bridgeID, err := pbx.CreateBridge(s.ctx)
	if err != nil {
		return err
	}
	s.bridgeMu.Lock()
	s.bridgeID = bridgeID
	s.bridgeMu.Unlock()

	if err := pbx.AddToBridge(s.ctx, bridgeID, s.channelID); err != nil {
		return err
	}

	externalHost := fmt.Sprintf("%s:%d", s.rtp.LocalHost(), s.rtp.LocalPort())
	media, err := pbx.CreateExternalMedia(s.ctx, externalHost, s.mediaID)
	if err != nil {
		return err
	}
	if err := s.applyPeer(media); err != nil {
		return err
	}

	select {
	case <-s.mediaReady:
	case <-time.After(mediaReadyTimeout):
	case <-s.ctx.Done():
		return nil
	}
	if s.ctx.Err() != nil {
		return nil
	}
	return pbx.AddToBridge(s.ctx, bridgeID, s.mediaID)
}

// applyPeer は ExternalMedia 側の RTP アドレスを、送信先として設定する。
func (s *Session) applyPeer(media *ari.Channel) error {
	addr := media.ChannelVars["UNICASTRTP_LOCAL_ADDRESS"]
	port := media.ChannelVars["UNICASTRTP_LOCAL_PORT"]
	if port == "" {
		var err error
		port, err = s.opts.ARI.ChannelVar(s.ctx, s.mediaID, "UNICASTRTP_LOCAL_PORT")
		if err != nil {
			return fmt.Errorf("RTPポート取得失敗: %w", err)
		}
	}
	if unusablePeerAddrs[addr] {
		addr = s.opts.FallbackPeerHost
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		// 送信先が決まらなくても、相手から届いた RTP の送信元アドレスで後から確定する
		slog.Warn("ExternalMedia のRTPポートが分からない", "channel", s.channelID, "port", port)
		return nil
	}
	return s.rtp.SetPeer(addr, portNum)
}
