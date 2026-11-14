// Command denwa-sts は ARI の着信音声を VAD で区切り、LFM3-Omni で返答テキストを生成して VOICEVOX で読み上げる。
//
// 起動: /home/denwa/sts/denwa-sts
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"denwa-sts/internal/app"
	"denwa-sts/internal/ari"
	"denwa-sts/internal/call"
	"denwa-sts/internal/config"
	"denwa-sts/internal/lfm"
	"denwa-sts/internal/voicevox"
)

func main() {
	if err := run(); err != nil {
		slog.Error("異常終了", "error", err)
		os.Exit(1)
	}
}

func run() error {
	server := flag.String("server", lfm.DefaultServerURL, "llama-server の /v1/realtime")
	instructions := flag.String("instructions", lfm.DefaultPhoneInstructions, "モデルへの指示")
	envPath := flag.String("env", config.EnvPath, "ARI 認証情報ファイルのパス")
	logPath := flag.String("log", config.DefaultLogPath, "ログファイルパス（空なら標準出力のみ）")
	flag.Parse()

	if err := config.SetupLogging(*logPath); err != nil {
		return err
	}
	creds, err := config.LoadARICredentials(*envPath)
	if err != nil {
		return err
	}

	synth, err := voicevox.Load()
	if err != nil {
		return err
	}
	defer synth.Close()
	slog.Info("VOICEVOX暖機")
	if _, err := synth.SynthesizeULaw("こんにちは"); err != nil {
		return fmt.Errorf("暖機失敗: %w", err)
	}
	slog.Info("VOICEVOX暖機完了")

	client := lfm.Client{
		ServerURL:    *server,
		Instructions: *instructions,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("起動", "app", config.AppName, "server", client.ServerURL, "log", *logPath)
	application := app.New(ari.NewClient(creds), call.Options{
		DialResponder: func(ctx context.Context) (call.Responder, error) {
			return client.Dial(ctx)
		},
		Synth:            synth,
		RTPBindHost:      config.RTPBindHost,
		FallbackPeerHost: config.ARIHost,
	})
	application.RunForever(ctx)
	slog.Info("停止")
	return nil
}
