package gemma

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionMultiTurnStream(t *testing.T) {
	var turns int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		case "/v1/chat/completions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("body: %v", err)
				return
			}
			var req struct {
				Messages  []chatMessage `json:"messages"`
				Stream    bool          `json:"stream"`
				MaxTokens int           `json:"max_tokens"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("json: %v", err)
				return
			}
			if !req.Stream {
				t.Error("stream=false")
			}
			if req.MaxTokens != maxReplyTokens {
				t.Errorf("max_tokens=%d", req.MaxTokens)
			}
			if len(req.Messages) < 2 || req.Messages[0].Role != "system" {
				t.Errorf("messages=%v", req.Messages)
			}
			turns++
			if turns == 2 {
				// 過去ターンの user は音声ではなくスタブ。最新 user だけ input_audio。
				audioMsgs := 0
				for _, msg := range req.Messages {
					raw, _ := json.Marshal(msg.Content)
					if strings.Contains(string(raw), "input_audio") {
						audioMsgs++
					}
				}
				if audioMsgs != 1 {
					t.Errorf("history audio msgs=%d body=%s", audioMsgs, string(body))
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			writeSSE(w, map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": "もしもし"}}},
			})
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := (Client{ServerURL: server.URL + "/v1/chat/completions"}).Dial(ctx)
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
	if turns != 2 {
		t.Fatalf("turns=%d", turns)
	}
	sess.mu.Lock()
	histLen := len(sess.history)
	sess.mu.Unlock()
	if histLen != 4 {
		t.Fatalf("history=%d", histLen)
	}
}

func TestStreamTextEmitsSentenceBeforeDone(t *testing.T) {
	sawFirst := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			return
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			writeSSE(w, map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": "こんにちは。"}}},
			})
			if flusher != nil {
				flusher.Flush()
			}
			writeSSE(w, map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": "明日は晴れです。"}}},
			})
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-sawFirst:
			case <-time.After(3 * time.Second):
				return
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-release
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := (Client{ServerURL: server.URL + "/v1/chat/completions"}).Dial(ctx)
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
		t.Fatal("完了より前に最初の文が出ていない")
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

func TestTrimHistory(t *testing.T) {
	history := []chatMessage{
		{Role: "user", Content: "1"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "2"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "3"},
		{Role: "assistant", Content: "c"},
	}
	got := trimHistory(history, 2)
	if len(got) != 4 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Content != "2" || got[3].Content != "c" {
		t.Fatalf("got=%v", got)
	}
}

func TestHealthURLFrom(t *testing.T) {
	got, err := healthURLFrom("http://127.0.0.1:8080/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080/health" {
		t.Fatalf("got=%q", got)
	}
}

func TestDeltaFromChunkIgnoresReasoning(t *testing.T) {
	payload := `{"choices":[{"delta":{"reasoning_content":"think","content":"はい"}}]}`
	got, err := deltaFromChunk(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got != "はい" {
		t.Fatalf("got=%q", got)
	}
}

func writeSSE(w http.ResponseWriter, value any) {
	payload, _ := json.Marshal(value)
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
}

func TestDialFailsOnBadHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := (Client{ServerURL: server.URL + "/v1/chat/completions"}).Dial(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "health") {
		t.Fatalf("err=%v", err)
	}
}

func TestWarmup(t *testing.T) {
	var gotReq struct {
		Stream             bool           `json:"stream"`
		MaxTokens          int            `json:"max_tokens"`
		Messages           []chatMessage  `json:"messages"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Errorf("json: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := (Client{ServerURL: server.URL + "/v1/chat/completions"}).Warmup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Stream {
		t.Error("stream=true")
	}
	if gotReq.MaxTokens != 1 {
		t.Errorf("max_tokens=%d", gotReq.MaxTokens)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" {
		t.Errorf("messages=%v", gotReq.Messages)
	}
	if gotReq.ChatTemplateKwargs["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs=%v", gotReq.ChatTemplateKwargs)
	}
}
