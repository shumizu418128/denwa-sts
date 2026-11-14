// Package gemma は llama-server の /v1/chat/completions で Gemma 4 の音声入力対話を行う。
package gemma

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"denwa-sts/internal/audio"
	"denwa-sts/internal/segment"
)

const (
	// DefaultServerURL は llama-server の既定の chat completions。
	DefaultServerURL = "http://127.0.0.1:8080/v1/chat/completions"
	// DefaultModel は llama-server に渡すモデル名。実体は起動時の -hf で決まる。
	DefaultModel = "gemma-4-e4b"
	// DefaultPhoneInstructions は電話対話向けの指示。
	DefaultPhoneInstructions = "You are on a phone call. Listen to the caller's audio and reply in plain spoken text only. Do not use markdown, bullet points, or special symbols. Reply in the same language the caller just used, either Japanese or English. Your reply will be read aloud by a text-to-speech engine, so keep sentences natural for speech."

	defaultInputRate = 16000
	// headerTimeout は応答ヘッダ待ち。本文ストリームは Client.Timeout せず context で打ち切る。
	headerTimeout = 60 * time.Second
	// maxHistoryTurns は保持する user/assistant の往復数。音声トークンが増えすぎないように抑える。
	maxHistoryTurns = 4
	userTurnPrompt  = "Reply to the caller now."
)

// Client は /v1/chat/completions への接続設定。
type Client struct {
	ServerURL    string
	Instructions string
	Model        string
	HTTPClient   *http.Client
}

// Session は通話中の会話履歴を保持し、発話ごとに返答テキストを返す。
type Session struct {
	endpoint     string
	model        string
	instructions string
	http         *http.Client

	mu        sync.Mutex
	closeOnce sync.Once
	closed    bool
	history   []chatMessage
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type contentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	InputAudio *inputAudio `json:"input_audio,omitempty"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// Dial は llama-server の survivability を確認し、通話用 Session を返す。
func (c Client) Dial(ctx context.Context) (*Session, error) {
	endpoint := c.ServerURL
	if endpoint == "" {
		endpoint = DefaultServerURL
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	instructions := c.Instructions
	if instructions == "" {
		instructions = DefaultPhoneInstructions
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout},
		}
	}

	healthURL, err := healthURLFrom(endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("health確認の準備に失敗した: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llama-serverへ接続できない: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("llama-serverのhealthが異常: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return &Session{
		endpoint:     endpoint,
		model:        model,
		instructions: instructions,
		http:         httpClient,
	}, nil
}

// Close はセッションを閉じる。何度呼んでもよい。
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.history = nil
		s.mu.Unlock()
	})
}

// RespondText は 1 発話分の PCM16 を送り、モデルが返したテキストを返す。
func (s *Session) RespondText(ctx context.Context, pcm []int16, sampleRate int) (string, error) {
	return s.respond(ctx, pcm, sampleRate, nil)
}

// StreamText は 1 発話分の PCM16 を送り、読み上げできる断片が揃うたびに onChunk を呼ぶ。
func (s *Session) StreamText(ctx context.Context, pcm []int16, sampleRate int, onChunk func(chunk string) error) (string, error) {
	if onChunk == nil {
		return "", errors.New("テキストの受け取り先がありません")
	}
	return s.respond(ctx, pcm, sampleRate, onChunk)
}

func (s *Session) respond(ctx context.Context, pcm []int16, sampleRate int, onChunk func(string) error) (string, error) {
	if len(pcm) == 0 {
		return "", errors.New("音声が空です")
	}
	if sampleRate <= 0 {
		return "", errors.New("サンプルレートが不正です")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("gemmaセッションは閉じている")
	}
	history := append([]chatMessage(nil), s.history...)
	s.mu.Unlock()

	resampled, err := audio.Resample(pcm, sampleRate, defaultInputRate)
	if err != nil {
		return "", err
	}
	if len(resampled) == 0 {
		return "", errors.New("リサンプル後の音声が空です")
	}
	wavB64 := base64.StdEncoding.EncodeToString(audio.EncodeWAV(resampled, defaultInputRate))
	userMsg := chatMessage{
		Role: "user",
		Content: []contentPart{
			{Type: "input_audio", InputAudio: &inputAudio{Data: wavB64, Format: "wav"}},
			{Type: "text", Text: userTurnPrompt},
		},
	}

	messages := make([]chatMessage, 0, 2+len(history))
	messages = append(messages, chatMessage{Role: "system", Content: s.instructions})
	messages = append(messages, history...)
	messages = append(messages, userMsg)

	body, err := json.Marshal(map[string]any{
		"model":    s.model,
		"messages": messages,
		"stream":   true,
		"chat_template_kwargs": map[string]any{
			"enable_thinking": false,
		},
	})
	if err != nil {
		return "", fmt.Errorf("リクエストJSONを作れない: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("chatリクエストの準備に失敗した: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chatリクエストに失敗した: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("chatがエラーを返した: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	text, err := s.readStream(ctx, resp.Body, onChunk)
	if err != nil {
		return text, err
	}
	if text == "" {
		return "", errors.New("返事の文字がありません")
	}

	s.mu.Lock()
	if !s.closed {
		s.history = append(s.history, userMsg, chatMessage{Role: "assistant", Content: text})
		s.history = trimHistory(s.history, maxHistoryTurns)
	}
	s.mu.Unlock()
	return text, nil
}

func (s *Session) readStream(ctx context.Context, body io.Reader, onChunk func(string) error) (string, error) {
	scanner := bufio.NewScanner(body)
	// 音声付き応答の SSE 行は長くなりうる。
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var text strings.Builder
	var seg segment.Segmenter
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			out := strings.TrimSpace(text.String())
			if onChunk != nil {
				_ = emitChunks(onChunk, seg.Flush())
			}
			return out, err
		}
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}

		delta, err := deltaFromChunk(payload)
		if err != nil {
			return strings.TrimSpace(text.String()), err
		}
		if delta == "" {
			continue
		}
		slog.Debug("モデルの文字", "text", delta)
		text.WriteString(delta)
		if onChunk == nil {
			continue
		}
		if err := emitChunks(onChunk, seg.Push(delta)); err != nil {
			return strings.TrimSpace(text.String()), err
		}
	}
	if err := scanner.Err(); err != nil {
		out := strings.TrimSpace(text.String())
		if onChunk != nil && ctx.Err() == nil {
			if chunkErr := emitChunks(onChunk, seg.Flush()); chunkErr != nil {
				return out, chunkErr
			}
		}
		return out, fmt.Errorf("chatストリームの受信に失敗した: %w", err)
	}

	out := strings.TrimSpace(text.String())
	if onChunk != nil {
		if err := emitChunks(onChunk, seg.Flush()); err != nil {
			return out, err
		}
	}
	return out, nil
}

func emitChunks(onChunk func(string) error, chunks []string) error {
	for _, chunk := range chunks {
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

func deltaFromChunk(payload string) (string, error) {
	var chunk struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return "", fmt.Errorf("chatストリームのJSONを読めない: %w", err)
	}
	if chunk.Error != nil && chunk.Error.Message != "" {
		return "", errors.New(chunk.Error.Message)
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}
	// thinking は捨てて、読み上げ用の本文だけ使う。
	return chunk.Choices[0].Delta.Content, nil
}

func trimHistory(history []chatMessage, maxTurns int) []chatMessage {
	if maxTurns <= 0 {
		return nil
	}
	maxMsgs := maxTurns * 2
	if len(history) <= maxMsgs {
		return history
	}
	return append([]chatMessage(nil), history[len(history)-maxMsgs:]...)
}

func healthURLFrom(chatURL string) (string, error) {
	u, err := url.Parse(chatURL)
	if err != nil {
		return "", fmt.Errorf("server URLが不正: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("server URLが不正")
	}
	u.Path = "/health"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
