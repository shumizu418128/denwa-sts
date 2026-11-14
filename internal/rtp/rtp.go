// Package rtp は1通話分の双方向 UnicastRTP セッションを扱う。
package rtp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"denwa-sts/internal/config"
)

const (
	headerLen            = 12
	peerWaitTimeout      = 3 * time.Second
	peerPollEvery        = 20 * time.Millisecond
	reportInterval       = 5 * time.Second
	maxPacketSize        = 2048
	streamPrebufferBytes = 0
)

// Session は UDP ソケット1つで RTP (PCMU) を送受信する。
type Session struct {
	conn      *net.UDPConn
	localHost string
	localPort int
	onULaw    func([]byte)

	peerMu sync.Mutex
	peer   *net.UDPAddr

	sendMu    sync.Mutex
	seq       uint16
	timestamp uint32
	ssrc      uint32

	stopOnce sync.Once
	stop     chan struct{}
	recvDone chan struct{}

	recvPackets atomic.Int64
	recvBytes   atomic.Int64
	txPackets   atomic.Int64
	txBytes     atomic.Int64
}

// NewSession は bindHost の空きポートに UDP ソケットを開く。
// onULaw は受信した μ-law ペイロードごとに呼ばれる。ペイロードの所有権は onULaw 側へ渡る。
func NewSession(bindHost string, onULaw func([]byte)) (*Session, error) {
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	local := conn.LocalAddr().(*net.UDPAddr)

	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	ssrc := binary.BigEndian.Uint32(random[6:])
	if ssrc == 0 {
		ssrc = 1
	}
	return &Session{
		conn:      conn,
		localHost: local.IP.String(),
		localPort: local.Port,
		onULaw:    onULaw,
		seq:       binary.BigEndian.Uint16(random[0:]),
		timestamp: binary.BigEndian.Uint32(random[2:]),
		ssrc:      ssrc,
		stop:      make(chan struct{}),
		recvDone:  make(chan struct{}),
	}, nil
}

// LocalHost は受信用に開いたアドレスを返す。
func (s *Session) LocalHost() string { return s.localHost }

// LocalPort は受信用に開いたポートを返す。
func (s *Session) LocalPort() int { return s.localPort }

// RecvPackets は受信したパケット数を返す。
func (s *Session) RecvPackets() int64 { return s.recvPackets.Load() }

// TxPackets は送信したパケット数を返す。
func (s *Session) TxPackets() int64 { return s.txPackets.Load() }

// --- 対向アドレス ---

// SetPeer は送信先を設定する。
func (s *Session) SetPeer(host string, port int) error {
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	s.peerMu.Lock()
	s.peer = addr
	s.peerMu.Unlock()
	slog.Info("RTP対向設定", "local", net.JoinHostPort(s.localHost, strconv.Itoa(s.localPort)), "peer", addr.String())
	return nil
}

func (s *Session) getPeer() *net.UDPAddr {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	return s.peer
}

func (s *Session) learnPeer(addr *net.UDPAddr) {
	s.peerMu.Lock()
	changed := s.peer == nil || !s.peer.IP.Equal(addr.IP) || s.peer.Port != addr.Port
	if changed {
		s.peer = addr
	}
	s.peerMu.Unlock()
	if changed {
		slog.Info("RTP対向確定", "peer", addr.String())
	}
}

// --- 開始 / 停止 ---

// Start は受信ループを始める。
func (s *Session) Start() {
	go s.recvLoop()
}

// Stop は送受信を止めてソケットを閉じる。何度呼んでもよい。
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		_ = s.conn.Close() // 受信待ちをエラーで戻す
	})
	select {
	case <-s.recvDone:
	case <-time.After(time.Second):
	}
}

func (s *Session) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// --- 受信 ---

func (s *Session) recvLoop() {
	defer close(s.recvDone)
	buf := make([]byte, maxPacketSize)
	lastReport := time.Now()
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if !s.stopped() {
				slog.Error("RTP受信失敗", "error", err)
			}
			return
		}
		s.learnPeer(addr)
		if n <= headerLen {
			continue
		}
		payload := make([]byte, n-headerLen)
		copy(payload, buf[headerLen:n])
		s.recvPackets.Add(1)
		s.recvBytes.Add(int64(len(payload)))
		if s.onULaw != nil {
			s.onULaw(payload)
		}
		if time.Since(lastReport) >= reportInterval {
			slog.Debug("RTP受信中", "packets", s.recvPackets.Load(), "bytes", s.recvBytes.Load())
			lastReport = time.Now()
		}
	}
}

// --- 送信 ---

func (s *Session) buildPacket(payload []byte, marker bool) []byte {
	packet := make([]byte, headerLen+len(payload))
	packet[0] = 0x80
	packet[1] = config.PayloadPCMU
	if marker {
		packet[1] |= 0x80
	}
	binary.BigEndian.PutUint16(packet[2:], s.seq)
	binary.BigEndian.PutUint32(packet[4:], s.timestamp)
	binary.BigEndian.PutUint32(packet[8:], s.ssrc)
	copy(packet[headerLen:], payload)
	s.seq++
	s.timestamp += uint32(len(payload))
	return packet
}

func (s *Session) waitPeer() *net.UDPAddr {
	deadline := time.Now().Add(peerWaitTimeout)
	for {
		if peer := s.getPeer(); peer != nil {
			return peer
		}
		if s.stopped() || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-s.stop:
			return nil
		case <-time.After(peerPollEvery):
		}
	}
}

// ErrNoPeer は送信先が分からないときのエラー。
var ErrNoPeer = errors.New("RTP対向アドレス不明のため送信不可")

// SendULaw は μ-law 音声を20ms間隔で全フレーム送出し切るまで待つ。
// 中断されたときは false を返す。
func (s *Session) SendULaw(payload []byte) bool {
	return s.SendULawContext(context.Background(), payload)
}

// SendULawContext は ctx が取り消されるか全フレーム送出し切るまで 20ms 間隔で送出する。
func (s *Session) SendULawContext(ctx context.Context, payload []byte) bool {
	ch := make(chan []byte, 1)
	ch <- payload
	close(ch)
	return s.sendStream(ctx, ch, 0)
}

// SendULawStream はチャンク列から届く μ-law 音声を、初期プリバッファ後に 20ms 間隔で送出する。
// 中断されたときは false を返す。
func (s *Session) SendULawStream(chunks <-chan []byte) bool {
	return s.SendULawStreamContext(context.Background(), chunks)
}

// SendULawStreamContext は ctx が取り消されるまでチャンク列の μ-law 音声を 20ms 間隔で送出する。
func (s *Session) SendULawStreamContext(ctx context.Context, chunks <-chan []byte) bool {
	return s.sendStream(ctx, chunks, streamPrebufferBytes)
}

func (s *Session) sendStream(ctx context.Context, chunks <-chan []byte, prebuffer int) bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	peer := s.waitPeer()
	if peer == nil {
		if !s.stopped() {
			slog.Error(ErrNoPeer.Error())
		}
		return false
	}

	var buf []byte
	offset := 0
	closed := false

	fillUntil := func(target int) bool {
		for !closed && len(buf)-offset < target {
			select {
			case <-s.stop:
				return false
			case <-ctx.Done():
				return false
			case chunk, ok := <-chunks:
				if !ok {
					closed = true
					return true
				}
				if len(chunk) == 0 {
					continue
				}
				if offset > 0 && offset >= len(buf)/2 {
					remaining := len(buf) - offset
					copy(buf[:remaining], buf[offset:])
					buf = buf[:remaining]
					offset = 0
				}
				buf = append(buf, chunk...)
			}
		}
		return true
	}

	if prebuffer > 0 {
		if !fillUntil(prebuffer) {
			return false
		}
	}

	nextSend := time.Now()
	marker := true
	for {
		if s.stopped() || ctx.Err() != nil {
			return false
		}
		if len(buf)-offset < config.FrameSamples && !closed {
			waited := time.Now()
			if !fillUntil(config.FrameSamples) {
				return false
			}
			// チャンク待ちで時間が飛んだときは送信時刻を合わせ直す
			if time.Since(waited) > config.FrameDuration {
				nextSend = time.Now()
			}
		}
		avail := len(buf) - offset
		if avail == 0 {
			if closed {
				return true
			}
			continue
		}

		frame := make([]byte, config.FrameSamples)
		n := copy(frame, buf[offset:])
		offset += n
		for i := n; i < len(frame); i++ {
			frame[i] = config.ULawSilence
		}

		if latest := s.getPeer(); latest != nil {
			peer = latest
		}
		if _, err := s.conn.WriteToUDP(s.buildPacket(frame, marker), peer); err != nil {
			if !s.stopped() {
				slog.Error("RTP送信失敗", "error", err)
			}
			return false
		}
		s.txPackets.Add(1)
		s.txBytes.Add(int64(len(frame)))
		marker = false

		nextSend = nextSend.Add(config.FrameDuration)
		if wait := time.Until(nextSend); wait > 0 {
			select {
			case <-s.stop:
				return false
			case <-ctx.Done():
				return false
			case <-time.After(wait):
			}
		}
	}
}
