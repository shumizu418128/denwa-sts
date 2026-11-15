// Package app は ARI イベントを受けて、着信ごとに call.Session を作る。
package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"denwa-sts/internal/ari"
	"denwa-sts/internal/call"
	"denwa-sts/internal/config"
)

const reconnectDelay = 2 * time.Second

// App は ARI のイベントループ。イベントは1つの goroutine で順に処理する。
type App struct {
	client  *ari.Client
	options call.Options

	sessions       map[string]*call.Session
	mediaToSession map[string]*call.Session
	activeCalls    atomic.Int64
}

// ActiveCalls は現在進行中の通話数を返す。
func (a *App) ActiveCalls() int {
	return int(a.activeCalls.Load())
}

// New は App を作る。options.ARI には client を使うので設定しなくてよい。
func New(client *ari.Client, options call.Options) *App {
	options.ARI = client
	return &App{
		client:         client,
		options:        options,
		sessions:       map[string]*call.Session{},
		mediaToSession: map[string]*call.Session{},
	}
}

// RunForever は ARI の WebSocket が切れたら、2秒後に再接続し続ける。ctx が終わると戻る。
func (a *App) RunForever(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Error("ARIイベントループ切断。再接続する", "delay", reconnectDelay, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// serve は1回分の接続で、切れるまでイベントを処理する。
func (a *App) serve(ctx context.Context) error {
	stream, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer a.cleanupAll()
	defer stream.Close()

	// ctx が終わったら、Recv で待っている処理を戻す
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			stream.Close()
		case <-stopWatch:
		}
	}()

	for {
		event, err := stream.Recv()
		if err != nil {
			return err
		}
		a.handleEvent(event)
	}
}

func (a *App) connect(ctx context.Context) (*ari.EventStream, error) {
	stream, err := a.client.OpenEvents(ctx)
	if err != nil {
		return nil, err
	}
	version, err := a.client.AsteriskVersion(ctx)
	if err != nil {
		stream.Close()
		return nil, err
	}
	slog.Info("ARI接続OK", "system", version, "app", config.AppName)
	return stream, nil
}

// --- イベント ---

func (a *App) handleEvent(event ari.Event) {
	if event.Type != "StasisStart" && event.Type != "StasisEnd" {
		return
	}
	if event.Channel == nil || event.Channel.ID == "" {
		return
	}
	channel := event.Channel

	switch {
	case channel.IsExternalMedia():
		if event.Type == "StasisStart" {
			if owner := a.mediaToSession[channel.ID]; owner != nil {
				owner.NotifyMediaReady()
			}
		}
	case event.Type == "StasisStart":
		a.onCallStart(channel)
	default:
		a.onCallEnd(channel.ID)
	}
}

func (a *App) onCallStart(channel *ari.Channel) {
	if _, exists := a.sessions[channel.ID]; exists {
		return
	}
	callerNum := channel.Caller.Number
	if callerNum == "" {
		callerNum = "?"
	}
	session, err := call.NewSession(a.options, channel.ID, callerNum)
	if err != nil {
		slog.Error("通話開始失敗", "channel", channel.ID, "error", err)
		a.hangup(channel.ID)
		return
	}
	a.sessions[channel.ID] = session
	a.mediaToSession[session.MediaID()] = session
	a.activeCalls.Store(int64(len(a.sessions)))
	session.Start()
}

func (a *App) onCallEnd(channelID string) {
	session := a.sessions[channelID]
	if session == nil {
		return
	}
	delete(a.sessions, channelID)
	delete(a.mediaToSession, session.MediaID())
	a.activeCalls.Store(int64(len(a.sessions)))
	slog.Info("相手切断 StasisEnd", "channel", channelID)
	// 後始末は ARI を呼ぶので、イベントの処理を止めないよう別の goroutine で行う
	go session.Cleanup()
}

func (a *App) hangup(channelID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.client.Hangup(ctx, channelID)
}

func (a *App) cleanupAll() {
	for _, session := range a.sessions {
		session.Cleanup()
	}
	clear(a.sessions)
	clear(a.mediaToSession)
	a.activeCalls.Store(0)
}
