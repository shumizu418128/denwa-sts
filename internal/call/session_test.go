package call

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"denwa-sts/internal/ari"
	"denwa-sts/internal/config"
	"denwa-sts/internal/rtp"
)

func newLoopbackRTP(t *testing.T, received chan<- []byte) (int, error) {
	t.Helper()
	peer, err := rtp.NewSession("127.0.0.1", func(payload []byte) {
		select {
		case received <- payload:
		default:
		}
	})
	if err != nil {
		return 0, err
	}
	peer.Start()
	t.Cleanup(peer.Stop)
	return peer.LocalPort(), nil
}

type fakeARI struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeARI) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeARI) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeARI) Answer(_ context.Context, id string) error { f.record("answer " + id); return nil }
func (f *fakeARI) Hangup(_ context.Context, id string)       { f.record("hangup " + id) }
func (f *fakeARI) ChannelVar(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeARI) CreateExternalMedia(_ context.Context, _, id string) (*ari.Channel, error) {
	f.record("externalMedia " + id)
	return &ari.Channel{ID: id, ChannelVars: map[string]string{
		"UNICASTRTP_LOCAL_ADDRESS": "0.0.0.0",
		"UNICASTRTP_LOCAL_PORT":    "20000",
	}}, nil
}
func (f *fakeARI) CreateBridge(context.Context) (string, error) {
	f.record("createBridge")
	return "br1", nil
}
func (f *fakeARI) AddToBridge(_ context.Context, bridge, id string) error {
	f.record("addToBridge " + bridge + " " + id)
	return nil
}
func (f *fakeARI) DestroyBridge(_ context.Context, id string) { f.record("destroyBridge " + id) }

type fakeResponder struct {
	mu    sync.Mutex
	turns int
}

func (f *fakeResponder) RespondText(_ context.Context, _ []int16, _ int) (string, error) {
	f.mu.Lock()
	f.turns++
	f.mu.Unlock()
	return "こんにちは", nil
}

func (f *fakeResponder) Close() {}

type fakeSynth struct {
	frames int
}

func (s fakeSynth) SynthesizeULaw(_ string) ([]byte, error) {
	n := s.frames
	if n <= 0 {
		n = 1
	}
	return make([]byte, config.FrameSamples*n), nil
}

func contains(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("待ち切れなかった: %s", what)
}

func TestCallLifecycle(t *testing.T) {
	pbx := &fakeARI{}
	res := &fakeResponder{}
	session, err := NewSession(Options{
		ARI:              pbx,
		DialResponder:    func(context.Context) (Responder, error) { return res, nil },
		RTPBindHost:      "127.0.0.1",
		FallbackPeerHost: config.ARIHost,
	}, "chan1", "100")
	if err != nil {
		t.Fatal(err)
	}
	session.Start()
	session.NotifyMediaReady()

	waitFor(t, "ExternalMedia をブリッジへ追加", func() bool {
		return contains(pbx.snapshot(), "addToBridge br1 extmedia-chan1")
	})
	calls := pbx.snapshot()
	for _, want := range []string{"answer chan1", "createBridge", "addToBridge br1 chan1", "externalMedia extmedia-chan1"} {
		if !contains(calls, want) {
			t.Errorf("%q が呼ばれていない: %v", want, calls)
		}
	}

	session.Cleanup()
	session.Cleanup() // 2回目で固まらないこと
	calls = pbx.snapshot()
	if !contains(calls, "hangup extmedia-chan1") || !contains(calls, "destroyBridge br1") {
		t.Errorf("後始末されていない: %v", calls)
	}
}

func TestCallSpeechRoundTrip(t *testing.T) {
	pbx := &fakeARI{}
	res := &fakeResponder{}

	// 送信された RTP を受けるループバック先を用意
	received := make(chan []byte, 8)
	peer, err := newLoopbackRTP(t, received)
	if err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(Options{
		ARI:              pbx,
		DialResponder:    func(context.Context) (Responder, error) { return res, nil },
		Synth:            fakeSynth{},
		RTPBindHost:      "127.0.0.1",
		FallbackPeerHost: "127.0.0.1",
	}, "chan2", "200")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Cleanup()

	if err := session.rtp.SetPeer("127.0.0.1", peer); err != nil {
		t.Fatal(err)
	}
	go session.respondLoop()

	session.enqueueUtterance([]int16{1000, -1000, 500, -500})

	select {
	case pkt := <-received:
		if len(pkt) != config.FrameSamples {
			t.Fatalf("frame len=%d", len(pkt))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("応答RTPパケットが届かない")
	}
}

func TestCallBargeInInterruptsPlayback(t *testing.T) {
	pbx := &fakeARI{}
	res := &fakeResponder{}

	received := make(chan []byte, 128)
	peer, err := newLoopbackRTP(t, received)
	if err != nil {
		t.Fatal(err)
	}

	// 1ターン目は50フレーム(1秒分)の長い音声を返す
	session, err := NewSession(Options{
		ARI:              pbx,
		DialResponder:    func(context.Context) (Responder, error) { return res, nil },
		Synth:            fakeSynth{frames: 50},
		RTPBindHost:      "127.0.0.1",
		FallbackPeerHost: "127.0.0.1",
	}, "chan3", "200")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Cleanup()

	if err := session.rtp.SetPeer("127.0.0.1", peer); err != nil {
		t.Fatal(err)
	}
	go session.respondLoop()

	session.enqueueUtterance([]int16{1000, -1000, 500, -500})
	// 最初の数パケットが届いた時点で割り込み発話開始を通知する
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("最初のRTPパケットが届かない")
	}
	t0 := time.Now()
	session.onCallerSpeechStart()
	// 割り込み後は1秒待たずにすぐターンが中断され、次の発話が処理されること
	session.enqueueUtterance([]int16{800, -800, 400, -400})
	waitFor(t, "2ターン目の処理開始", func() bool {
		res.mu.Lock()
		defer res.mu.Unlock()
		return res.turns >= 2
	})
	if elapsed := time.Since(t0); elapsed > 400*time.Millisecond {
		t.Fatalf("割り込み中断が遅い: %v", elapsed)
	}
}

type scriptedStreamer struct {
	chunks      []string
	startedRest chan struct{}
	releaseRest chan struct{}
}

func (s *scriptedStreamer) RespondText(context.Context, []int16, int) (string, error) {
	return "", nil
}

func (s *scriptedStreamer) Close() {}

func (s *scriptedStreamer) StreamText(ctx context.Context, _ []int16, _ int, onChunk func(string) error) (string, error) {
	var b strings.Builder
	for i, chunk := range s.chunks {
		if err := onChunk(chunk); err != nil {
			return b.String(), err
		}
		b.WriteString(chunk)
		if i == 0 {
			close(s.startedRest)
			select {
			case <-ctx.Done():
				return b.String(), ctx.Err()
			case <-s.releaseRest:
			}
		}
	}
	return b.String(), nil
}

type gateSynth struct {
	mu           sync.Mutex
	texts        []string
	firstStarted chan struct{}
	holdFirst    chan struct{}
}

func (g *gateSynth) SynthesizeULaw(text string) ([]byte, error) {
	g.mu.Lock()
	g.texts = append(g.texts, text)
	n := len(g.texts)
	g.mu.Unlock()
	if n == 1 {
		close(g.firstStarted)
		<-g.holdFirst
	}
	return make([]byte, config.FrameSamples), nil
}

func (g *gateSynth) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.texts...)
}

func TestCallStartsSpeechBeforeTextDone(t *testing.T) {
	streamer := &scriptedStreamer{
		chunks:      []string{"前半です。", "後半です。"},
		startedRest: make(chan struct{}),
		releaseRest: make(chan struct{}),
	}
	synth := &gateSynth{
		firstStarted: make(chan struct{}),
		holdFirst:    make(chan struct{}),
	}
	release := sync.OnceFunc(func() {
		close(synth.holdFirst)
		close(streamer.releaseRest)
	})
	t.Cleanup(release)

	received := make(chan []byte, 128)
	peer, err := newLoopbackRTP(t, received)
	if err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(Options{
		ARI:              &fakeARI{},
		DialResponder:    func(context.Context) (Responder, error) { return streamer, nil },
		Synth:            synth,
		RTPBindHost:      "127.0.0.1",
		FallbackPeerHost: "127.0.0.1",
	}, "chan4", "200")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Cleanup()
	if err := session.rtp.SetPeer("127.0.0.1", peer); err != nil {
		t.Fatal(err)
	}
	go session.respondLoop()
	session.enqueueUtterance([]int16{1000, -1000, 500, -500})

	select {
	case <-synth.firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("最初の断片の音声合成が始まらない")
	}
	select {
	case <-streamer.startedRest:
	case <-time.After(3 * time.Second):
		t.Fatal("最初の断片のあとテキスト生成が止まっている")
	}
	got := synth.snapshot()
	if len(got) != 1 || got[0] != "前半です。" {
		t.Fatalf("音声合成を待たずに次の文へ進めていない: %q", got)
	}

	release()
	waitFor(t, "2断片目の音声合成", func() bool {
		return len(synth.snapshot()) >= 2
	})
	got = synth.snapshot()
	if len(got) != 2 || got[0] != "前半です。" || got[1] != "後半です。" {
		t.Fatalf("合成順=%q", got)
	}
}

func TestCallPlaysFirstChunkBeforeTextDone(t *testing.T) {
	const marker = 0x10
	streamer := &scriptedStreamer{
		chunks:      []string{"前半です。", "後半です。"},
		startedRest: make(chan struct{}),
		releaseRest: make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(streamer.releaseRest) })
	t.Cleanup(release)

	received := make(chan []byte, 128)
	peer, err := newLoopbackRTP(t, received)
	if err != nil {
		t.Fatal(err)
	}

	session, err := NewSession(Options{
		ARI:              &fakeARI{},
		DialResponder:    func(context.Context) (Responder, error) { return streamer, nil },
		Synth:            markerSynth{marker: marker},
		RTPBindHost:      "127.0.0.1",
		FallbackPeerHost: "127.0.0.1",
	}, "chan5", "200")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Cleanup()
	if err := session.rtp.SetPeer("127.0.0.1", peer); err != nil {
		t.Fatal(err)
	}
	go session.respondLoop()
	session.enqueueUtterance([]int16{1000, -1000, 500, -500})

	select {
	case <-streamer.startedRest:
	case <-time.After(3 * time.Second):
		t.Fatal("最初の断片が出ていない")
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case pkt := <-received:
			if len(pkt) == config.FrameSamples && bytes.Count(pkt, []byte{marker}) == len(pkt) {
				release()
				return
			}
		case <-deadline:
			t.Fatal("残りの文章が終わる前に最初の音声が流れない")
		}
	}
}

type markerSynth struct {
	marker byte
}

func (s markerSynth) SynthesizeULaw(_ string) ([]byte, error) {
	return bytes.Repeat([]byte{s.marker}, config.FrameSamples), nil
}
