package rtp

import (
	"bytes"
	"testing"
	"time"

	"denwa-sts/internal/config"
)

func TestSendAndReceiveOverLoopback(t *testing.T) {
	received := make(chan []byte, 8)
	receiver, err := NewSession("127.0.0.1", func(payload []byte) { received <- payload })
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	receiver.Start()

	sender, err := NewSession("127.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Stop()
	if err := sender.SetPeer("127.0.0.1", receiver.LocalPort()); err != nil {
		t.Fatal(err)
	}

	// 1.5 フレーム分。2つ目は無音で埋められる
	payload := bytes.Repeat([]byte{0x7F}, config.FrameSamples+config.FrameSamples/2)
	if !sender.SendULaw(payload) {
		t.Fatal("送信に失敗")
	}

	for i := 0; i < 2; i++ {
		select {
		case got := <-received:
			if len(got) != config.FrameSamples {
				t.Fatalf("frame %d len=%d", i, len(got))
			}
		case <-time.After(time.Second):
			t.Fatalf("frame %d が届かない", i)
		}
	}
	if receiver.RecvPackets() != 2 {
		t.Fatalf("packets=%d", receiver.RecvPackets())
	}
}

func TestSendStreamOverLoopback(t *testing.T) {
	received := make(chan []byte, 16)
	receiver, err := NewSession("127.0.0.1", func(payload []byte) { received <- payload })
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	receiver.Start()

	sender, err := NewSession("127.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Stop()
	if err := sender.SetPeer("127.0.0.1", receiver.LocalPort()); err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte, 4)
	chunks <- bytes.Repeat([]byte{0x10}, 200)
	chunks <- bytes.Repeat([]byte{0x20}, 200)
	close(chunks)

	if !sender.SendULawStream(chunks) {
		t.Fatal("ストリーム送信に失敗")
	}
	// 400 bytes -> 3 packets (160, 160, 80+80 silence)
	if receiver.RecvPackets() != 3 {
		t.Fatalf("packets=%d", receiver.RecvPackets())
	}
}

func TestSendStreamUnderrunKeepsPace(t *testing.T) {
	type packet struct {
		at   time.Time
		data []byte
	}
	received := make(chan packet, 32)
	receiver, err := NewSession("127.0.0.1", func(payload []byte) {
		received <- packet{at: time.Now(), data: append([]byte(nil), payload...)}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	receiver.Start()

	sender, err := NewSession("127.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Stop()
	if err := sender.SetPeer("127.0.0.1", receiver.LocalPort()); err != nil {
		t.Fatal(err)
	}

	chunks := make(chan []byte)
	done := make(chan bool, 1)
	go func() {
		done <- sender.SendULawStream(chunks)
	}()

	chunks <- bytes.Repeat([]byte{0x10}, config.FrameSamples)
	time.Sleep(120 * time.Millisecond)
	chunks <- bytes.Repeat([]byte{0x20}, config.FrameSamples)
	close(chunks)

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("ストリーム送信に失敗")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ストリーム送信が終わらない")
	}

	var packets []packet
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case pkt := <-received:
			packets = append(packets, pkt)
		case <-timeout:
			goto collected
		}
	}
collected:
	silence := 0
	sawFirst := false
	sawSecond := false
	for _, pkt := range packets {
		if len(pkt.data) != config.FrameSamples {
			t.Fatalf("frame len=%d", len(pkt.data))
		}
		switch pkt.data[0] {
		case 0x10:
			if sawSecond {
				t.Fatal("後続の音声のあとに先頭フレームが届いた")
			}
			sawFirst = true
		case 0x20:
			if !sawFirst || silence == 0 {
				t.Fatal("欠落のあいだに無音フレームがない")
			}
			sawSecond = true
		case config.ULawSilence:
			if sawFirst && !sawSecond {
				silence++
			}
		default:
			t.Fatalf("予期しないバイト %#x", pkt.data[0])
		}
	}
	if !sawSecond || silence < 2 {
		t.Fatalf("packets=%d silence=%d", len(packets), silence)
	}
	for i := 1; i < len(packets); i++ {
		gap := packets[i].at.Sub(packets[i-1].at)
		if gap > 80*time.Millisecond {
			t.Fatalf("パケット間隔が空きすぎ: %s", gap)
		}
	}
}

func TestSendWithoutPeerFailsAfterStop(t *testing.T) {
	session, err := NewSession("127.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	session.Start()
	go func() {
		time.Sleep(50 * time.Millisecond)
		session.Stop()
	}()
	if session.SendULaw(make([]byte, config.FrameSamples)) {
		t.Fatal("対向不明なのに成功した")
	}
}
