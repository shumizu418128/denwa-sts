package lfm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"denwa-sts/internal/audio"

	"github.com/gorilla/websocket"
)

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
