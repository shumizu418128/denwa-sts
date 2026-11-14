// Package lfm は llama-server の /v1/realtime と音声対話を行う。
package lfm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"denwa-sts/internal/audio"
	"denwa-sts/internal/segment"

	"github.com/gorilla/websocket"
)

const (
	// DefaultServerURL は llama-server の既定の待ち受け。
	DefaultServerURL = "ws://127.0.0.1:8080/v1/realtime"
	// DefaultPhoneInstructions は電話対話向けの指示。
	// 先頭の "Respond in text." が無いと、LFM3-Omni は返事をせず入力を繰り返す。
	DefaultPhoneInstructions = "Respond in text. Speak naturally, as if you were the other person on a phone call. Do not repeat what the other person says; respond directly to the content. Reply in the same language the caller just used, either Japanese or English. Your responses will be read aloud by a text-to-speech engine, so generate text that sounds natural when spoken."

	defaultInputRate = 16000
	readLimit        = 32 << 20
	dialTimeout      = 10 * time.Second
)

// Client は /v1/realtime への接続設定。
type Client struct {
	ServerURL    string
	Instructions string
}

// Session は 1 本の WebSocket で複数ターンの会話を行う。
type Session struct {
	conn   *websocket.Conn
	inRate int

	mu        sync.Mutex
	closeOnce sync.Once
	closed    bool
}

// Dial は /v1/realtime に接続し、session.created と session.update を完了した Session を返す。
func (c Client) Dial(ctx context.Context) (*Session, error) {
	serverURL := c.ServerURL
	if serverURL == "" {
		serverURL = DefaultServerURL
	}
	instructions := c.Instructions
	if instructions == "" {
		instructions = DefaultPhoneInstructions
	}

	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	conn, _, err := dialer.DialContext(ctx, serverURL, nil)
	if err != nil {
		return nil, fmt.Errorf("realtimeへ接続できない: %w", err)
	}
	conn.SetReadLimit(readLimit)

	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()
	stopWatching := func() {
		close(stopWatch)
		<-watchDone
	}

	created, err := readUntil(conn, "session.created")
	if err != nil {
		stopWatching()
		_ = conn.Close()
		return nil, err
	}
	inRate := ratesFromSession(created.Session)

	if err := writeJSON(conn, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions":      instructions,
			"output_modalities": []string{"text"},
		},
	}); err != nil {
		stopWatching()
		_ = conn.Close()
		return nil, err
	}
	if _, err := readUntil(conn, "session.updated"); err != nil {
		stopWatching()
		_ = conn.Close()
		return nil, err
	}
	stopWatching()

	return &Session{
		conn:   conn,
		inRate: inRate,
	}, nil
}

// Close は WebSocket 接続を閉じる。何度呼んでもよい。
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		_ = s.conn.Close()
	})
}

// RespondText は 1 発話分の PCM16 を送り、モデルが返したテキストを返す。
func (s *Session) RespondText(ctx context.Context, pcm []int16, sampleRate int) (string, error) {
	return s.respond(ctx, pcm, sampleRate, nil)
}

// StreamText は 1 発話分の PCM16 を送り、読み上げできる断片が揃うたびに onChunk を呼ぶ。
// 全文の生成完了を待たない。戻り値は返答全文。
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
		return "", errors.New("realtimeセッションは閉じている")
	}
	s.mu.Unlock()

	resampled, err := audio.Resample(pcm, sampleRate, s.inRate)
	if err != nil {
		return "", err
	}
	if len(resampled) == 0 {
		return "", errors.New("リサンプル後の音声が空です")
	}

	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			s.Close()
		case <-stopWatch:
		}
	}()
	defer func() {
		close(stopWatch)
		<-watchDone
	}()

	if err := writeJSON(s.conn, map[string]any{"type": "input_audio_buffer.clear"}); err != nil {
		return "", err
	}
	samplesPerChunk := int(time.Duration(s.inRate) * chunkDuration / time.Second)
	if samplesPerChunk < 1 {
		samplesPerChunk = 1
	}
	for start := 0; start < len(resampled); start += samplesPerChunk {
		end := start + samplesPerChunk
		if end > len(resampled) {
			end = len(resampled)
		}
		payload := audio.PCM16Bytes(resampled[start:end])
		if err := writeJSON(s.conn, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(payload),
		}); err != nil {
			return "", err
		}
	}
	if err := writeJSON(s.conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return "", err
	}
	if err := writeJSON(s.conn, map[string]any{"type": "response.create"}); err != nil {
		return "", err
	}

	var text strings.Builder
	var seg segment.Segmenter
	for {
		event, err := readEvent(s.conn)
		if err != nil {
			out := strings.TrimSpace(text.String())
			if onChunk != nil && ctx.Err() == nil {
				if chunkErr := emitChunks(onChunk, seg.Flush()); chunkErr != nil {
					return out, chunkErr
				}
			}
			return out, err
		}
		switch event.Type {
		case "error":
			message := "realtimeがエラーを返した"
			if event.Error != nil && event.Error.Message != "" {
				message = event.Error.Message
			}
			return strings.TrimSpace(text.String()), errors.New(message)
		case "response.output_text.delta":
			if event.Delta == "" {
				continue
			}
			slog.Debug("モデルの文字", "text", event.Delta)
			text.WriteString(event.Delta)
			if onChunk == nil {
				continue
			}
			if err := emitChunks(onChunk, seg.Push(event.Delta)); err != nil {
				return strings.TrimSpace(text.String()), err
			}
		case "response.done":
			if status := responseStatus(event.Response); status != "" && status != "completed" {
				return strings.TrimSpace(text.String()), fmt.Errorf("返事が完了していない: %s", status)
			}
			out := strings.TrimSpace(text.String())
			if out == "" {
				return "", errors.New("返事の文字がありません")
			}
			if onChunk != nil {
				if err := emitChunks(onChunk, seg.Flush()); err != nil {
					return out, err
				}
			}
			return out, nil
		}
	}
}

func emitChunks(onChunk func(string) error, chunks []string) error {
	for _, chunk := range chunks {
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

type incoming struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Session  json.RawMessage `json:"session"`
	Response json.RawMessage `json:"response"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func readUntil(conn *websocket.Conn, want string) (incoming, error) {
	for {
		event, err := readEvent(conn)
		if err != nil {
			return incoming{}, err
		}
		if event.Type == "error" {
			message := "realtimeがエラーを返した"
			if event.Error != nil && event.Error.Message != "" {
				message = event.Error.Message
			}
			return incoming{}, errors.New(message)
		}
		if event.Type == want {
			return event, nil
		}
	}
}

func readEvent(conn *websocket.Conn) (incoming, error) {
	_, payload, err := conn.ReadMessage()
	if err != nil {
		return incoming{}, fmt.Errorf("realtimeの受信に失敗した: %w", err)
	}
	var event incoming
	if err := json.Unmarshal(payload, &event); err != nil {
		return incoming{}, fmt.Errorf("realtimeのJSONを読めない: %w", err)
	}
	return event, nil
}

func writeJSON(conn *websocket.Conn, value any) error {
	if err := conn.WriteJSON(value); err != nil {
		return fmt.Errorf("realtimeへ送信できない: %w", err)
	}
	return nil
}

func ratesFromSession(raw json.RawMessage) int {
	inRate := defaultInputRate
	var session struct {
		Audio struct {
			Input struct {
				Format struct {
					Rate int `json:"rate"`
				} `json:"format"`
			} `json:"input"`
		} `json:"audio"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &session) != nil {
		return inRate
	}
	if session.Audio.Input.Format.Rate > 0 {
		inRate = session.Audio.Input.Format.Rate
	}
	return inRate
}

func responseStatus(raw json.RawMessage) string {
	var response struct {
		Status string `json:"status"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		return ""
	}
	return response.Status
}

// chunkDuration は 1 回の append の長さ。約 100 ms。
const chunkDuration = 100 * time.Millisecond
