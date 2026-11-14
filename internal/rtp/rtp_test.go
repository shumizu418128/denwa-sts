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
