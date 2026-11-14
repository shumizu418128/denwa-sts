// Package ari は Asterisk ARI のクライアント。REST は net/http、イベントは WebSocket。
package ari

import (
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
	"time"

	"github.com/gorilla/websocket"

	"denwa-sts/internal/config"
)

const (
	requestTimeout   = 10 * time.Second
	wsConnectTimeout = 10 * time.Second

	addToBridgeRetries = 25
	addToBridgeDelay   = 40 * time.Millisecond
)

// Channel は ARI のチャネル。使う項目だけを持つ。
type Channel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Caller struct {
		Number string `json:"number"`
	} `json:"caller"`
	ChannelVars map[string]string `json:"channelvars"`
}

// IsExternalMedia は ExternalMedia が作ったチャネルかどうかを返す。
func (c *Channel) IsExternalMedia() bool {
	return strings.HasPrefix(c.Name, "UnicastRTP/")
}

// Event は ARI の WebSocket イベント。
type Event struct {
	Type    string   `json:"type"`
	Channel *Channel `json:"channel"`
}

// HTTPError は ARI が 2xx 以外を返したときのエラー。
type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("ARI %s %s -> %d %s", e.Method, e.URL, e.StatusCode, e.Body)
}

func statusCode(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

// Client は ARI の REST クライアント。
type Client struct {
	user     string
	password string
	baseURL  string
	wsURL    string
	http     *http.Client
}

// NewClient は設定の ARI 接続先に向けたクライアントを作る。
func NewClient(creds config.ARICredentials) *Client {
	return &Client{
		user:     creds.User,
		password: creds.Password,
		baseURL:  config.ARIBaseURL,
		wsURL:    config.ARIWSURL,
		http:     &http.Client{Timeout: requestTimeout},
	}
}

// Request は ARI を呼び、2xx の本文を返す。本文がなければ nil を返す。
func (c *Client) Request(ctx context.Context, method, path string, query url.Values) ([]byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.password)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpErr := &HTTPError{Method: method, URL: target, StatusCode: resp.StatusCode, Body: string(body)}
		slog.Error("ARIエラー", "method", method, "url", target, "status", resp.StatusCode, "body", string(body))
		return nil, httpErr
	}
	if len(body) == 0 {
		return nil, nil
	}
	return body, nil
}

// AsteriskVersion は /asterisk/info から Asterisk のバージョンを取る。
func (c *Client) AsteriskVersion(ctx context.Context) (string, error) {
	body, err := c.Request(ctx, http.MethodGet, "/asterisk/info", nil)
	if err != nil {
		return "", err
	}
	var info struct {
		System struct {
			Version string `json:"version"`
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	if info.System.Version == "" {
		return "?", nil
	}
	return info.System.Version, nil
}

// --- イベント ---

// EventStream は ARI の WebSocket イベント。切断されると Recv がエラーを返す。
type EventStream struct {
	conn *websocket.Conn
}

// OpenEvents は WebSocket に接続する。
func (c *Client) OpenEvents(ctx context.Context) (*EventStream, error) {
	target := c.wsURL + "?" + url.Values{"app": {config.AppName}}.Encode()
	token := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.password))
	dialer := websocket.Dialer{HandshakeTimeout: wsConnectTimeout}
	conn, _, err := dialer.DialContext(ctx, target, http.Header{"Authorization": {"Basic " + token}})
	if err != nil {
		return nil, err
	}
	slog.Info("ARI WebSocket接続完了", "url", target)
	return &EventStream{conn: conn}, nil
}

// Recv は次のイベントを待つ。
func (s *EventStream) Recv() (Event, error) {
	_, message, err := s.conn.ReadMessage()
	if err != nil {
		return Event{}, fmt.Errorf("WebSocket切断: %w", err)
	}
	var event Event
	if err := json.Unmarshal(message, &event); err != nil {
		return Event{}, fmt.Errorf("イベントの解析失敗: %w", err)
	}
	return event, nil
}

// Close は WebSocket を閉じる。Recv で待っている処理はエラーで戻る。
func (s *EventStream) Close() {
	_ = s.conn.Close()
}

// --- チャネル ---

func escape(id string) string {
	return url.PathEscape(id)
}

// Answer はチャネルに応答する。
func (c *Client) Answer(ctx context.Context, channelID string) error {
	_, err := c.Request(ctx, http.MethodPost, "/channels/"+escape(channelID)+"/answer", nil)
	return err
}

// Hangup はチャネルを切る。すでにない (404) ときは何もしない。
func (c *Client) Hangup(ctx context.Context, channelID string) {
	_, err := c.Request(ctx, http.MethodDelete, "/channels/"+escape(channelID), nil)
	if err != nil && statusCode(err) != http.StatusNotFound {
		slog.Warn("hangup失敗", "channel", channelID, "error", err)
	}
}

// ChannelVar はチャネル変数を読む。
func (c *Client) ChannelVar(ctx context.Context, channelID, variable string) (string, error) {
	body, err := c.Request(ctx, http.MethodGet, "/channels/"+escape(channelID)+"/variable",
		url.Values{"variable": {variable}})
	if err != nil || body == nil {
		return "", err
	}
	var res struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", err
	}
	return res.Value, nil
}

// CreateExternalMedia は RTP の送受信先を externalHost にした ExternalMedia チャネルを作る。
func (c *Client) CreateExternalMedia(ctx context.Context, externalHost, channelID string) (*Channel, error) {
	body, err := c.Request(ctx, http.MethodPost, "/channels/externalMedia", url.Values{
		"channelId":       {channelID},
		"app":             {config.AppName},
		"external_host":   {externalHost},
		"format":          {"ulaw"},
		"encapsulation":   {"rtp"},
		"transport":       {"udp"},
		"connection_type": {"client"},
		"direction":       {"both"},
	})
	if err != nil {
		return nil, err
	}
	var channel Channel
	if err := json.Unmarshal(body, &channel); err != nil {
		return nil, fmt.Errorf("ExternalMediaの応答を解析できない: %w", err)
	}
	slog.Info("ExternalMedia作成", "id", channel.ID, "name", channel.Name)
	return &channel, nil
}

// --- ブリッジ ---

// CreateBridge は mixing ブリッジを作り、その ID を返す。
func (c *Client) CreateBridge(ctx context.Context) (string, error) {
	body, err := c.Request(ctx, http.MethodPost, "/bridges", url.Values{"type": {"mixing"}})
	if err != nil {
		return "", err
	}
	var bridge struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &bridge); err != nil || bridge.ID == "" {
		return "", fmt.Errorf("bridgeの応答を解析できない: %w", err)
	}
	slog.Info("bridge作成", "id", bridge.ID)
	return bridge.ID, nil
}

// AddToBridge はチャネルをブリッジへ入れる。
// チャネルが Stasis に入る前は 422 になるので、少し待って再試行する。
func (c *Client) AddToBridge(ctx context.Context, bridgeID, channelID string) error {
	path := "/bridges/" + escape(bridgeID) + "/addChannel"
	for attempt := 1; ; attempt++ {
		_, err := c.Request(ctx, http.MethodPost, path, url.Values{"channel": {channelID}})
		if err == nil {
			return nil
		}
		if statusCode(err) != http.StatusUnprocessableEntity || attempt >= addToBridgeRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(addToBridgeDelay):
		}
	}
}

// DestroyBridge はブリッジを消す。すでにない (404) ときは何もしない。
func (c *Client) DestroyBridge(ctx context.Context, bridgeID string) {
	_, err := c.Request(ctx, http.MethodDelete, "/bridges/"+escape(bridgeID), nil)
	if err != nil && statusCode(err) != http.StatusNotFound {
		slog.Warn("bridge削除失敗", "id", bridgeID, "error", err)
	}
}
