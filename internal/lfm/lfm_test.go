package lfm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"denwa-sts/internal/audio"

	"github.com/gorilla/websocket"
)

func TestTurnWritesReplyWAV(t *testing.T) {
	var appendBytes [][]byte
	var order []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type": "session.created",
			"session": map[string]any{
				"audio": map[string]any{
					"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 16000}},
					"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				},
			},
		})
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event struct {
				Type  string `json:"type"`
				Audio string `json:"audio"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				return
			}
			order = append(order, event.Type)
			if event.Type == "input_audio_buffer.append" {
				raw, err := base64.StdEncoding.DecodeString(event.Audio)
				if err != nil || len(raw)%2 != 0 {
					t.Errorf("append のバイト数が偶数ではない len=%d err=%v", len(raw), err)
					return
				}
				appendBytes = append(appendBytes, raw)
			}
			if event.Type == "response.create" {
				break
			}
		}
		reply := audio.PCM16Bytes([]int16{1000, -1000})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "こんにちは"})
		_ = conn.WriteJSON(map[string]any{
			"type":  "response.output_audio.delta",
			"delta": base64.StdEncoding.EncodeToString(reply),
		})
		_ = conn.WriteJSON(map[string]any{
			"type":     "response.done",
			"response": map[string]any{"status": "completed"},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := Client{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http")}
	got, err := client.Turn(ctx, []int16{0, 1000, -1000, 500}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "こんにちは" {
		t.Fatalf("text=%q", got.Text)
	}
	if got.SampleRate != 24000 {
		t.Fatalf("rate=%d", got.SampleRate)
	}
	if len(got.PCM) != 2 || got.PCM[0] != 1000 || got.PCM[1] != -1000 {
		t.Fatalf("pcm=%v", got.PCM)
	}
	createAt := -1
	appendAt := -1
	for i, kind := range order {
		if kind == "input_audio_buffer.append" && appendAt < 0 {
			appendAt = i
		}
		if kind == "response.create" {
			createAt = i
		}
	}
	if appendAt < 0 || createAt < appendAt {
		t.Fatalf("order=%v", order)
	}
	if len(appendBytes) == 0 {
		t.Fatal("append がない")
	}

	path := filepath.Join(t.TempDir(), "answer.wav")
	if err := audio.WriteWAV(path, got.PCM, got.SampleRate); err != nil {
		t.Fatal(err)
	}
	readBack, rate, err := audio.ReadWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 24000 || len(readBack) != 2 || readBack[0] != 1000 {
		t.Fatalf("wav rate=%d pcm=%v", rate, readBack)
	}
}

func TestTurnReturnsServerError(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type": "error",
			"error": map[string]any{
				"message": "モデルが無い",
			},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := Client{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http")}
	_, err := client.Turn(ctx, []int16{1, 2}, 16000)
	if err == nil || !strings.Contains(err.Error(), "モデルが無い") {
		t.Fatalf("err=%v", err)
	}
}

func TestSessionMultiTurnStream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type": "session.created",
			"session": map[string]any{
				"audio": map[string]any{
					"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 16000}},
					"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				},
			},
		})
		for turn := 0; turn < 2; turn++ {
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var event struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(payload, &event)
				if event.Type == "session.update" {
					_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
				}
				if event.Type == "response.create" {
					break
				}
			}
			_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "もしもし"})
			_ = conn.WriteJSON(map[string]any{
				"type":  "response.output_audio.delta",
				"delta": base64.StdEncoding.EncodeToString(audio.PCM16Bytes([]int16{100, -100, 200})),
			})
			_ = conn.WriteJSON(map[string]any{
				"type":     "response.done",
				"response": map[string]any{"status": "completed"},
			})
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := (Client{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http")}).Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	for i := 0; i < 2; i++ {
		text, err := sess.RespondText(ctx, []int16{10, 20, 30, 40}, 8000)
		if err != nil {
			t.Fatalf("turn %d err=%v", i, err)
		}
		if text != "もしもし" {
			t.Fatalf("turn %d text=%q", i, text)
		}
	}
}

func TestStreamTextEmitsSentenceBeforeDone(t *testing.T) {
	sawFirst := make(chan struct{})
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"type": "session.created",
			"session": map[string]any{
				"audio": map[string]any{
					"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 16000}},
					"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				},
			},
		})
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload, &event)
			if event.Type == "session.update" {
				_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
			}
			if event.Type == "response.create" {
				break
			}
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "こんにちは。"})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "明日は晴れです。"})
		select {
		case <-sawFirst:
		case <-time.After(3 * time.Second):
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type":     "response.done",
			"response": map[string]any{"status": "completed"},
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := (Client{ServerURL: "ws" + strings.TrimPrefix(server.URL, "http")}).Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	var got []string
	go func() {
		text, err := sess.StreamText(ctx, []int16{10, 20, 30, 40}, 8000, func(chunk string) error {
			got = append(got, chunk)
			if len(got) == 1 {
				close(sawFirst)
				<-release
			}
			return nil
		})
		done <- result{text, err}
	}()

	select {
	case <-sawFirst:
	case <-time.After(3 * time.Second):
		t.Fatal("response.done より前に最初の文が出ていない")
	}
	close(release)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.text != "こんにちは。明日は晴れです。" {
			t.Fatalf("text=%q", res.text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StreamText が戻らない")
	}
	if len(got) != 2 || got[0] != "こんにちは。" || got[1] != "明日は晴れです。" {
		t.Fatalf("chunks=%q", got)
	}
}
