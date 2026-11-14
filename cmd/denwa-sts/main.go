// Command denwa-sts は ARI の着信音声を VAD で区切り、Gemma 4 E4B で返答テキストを生成して読み上げる。
// 日本語はずんだもん、英語は Piper。
//
// 起動: /home/denwa/sts/denwa-sts
package main

import (
	"bytes"
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
	"denwa-sts/internal/gemma"
	"denwa-sts/internal/synth"
)

func main() {
	if err := run(); err != nil {
		slog.Error("異常終了", "error", err)
		os.Exit(1)
	}
}

func run() error {
	server := flag.String("server", gemma.DefaultServerURL, "llama-server の /v1/chat/completions")
	model := flag.String("model", gemma.DefaultModel, "llama-server に渡すモデル名")
	instructions := flag.String("instructions", gemma.DefaultPhoneInstructions, "モデルへの指示")
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

	engine, err := synth.Load()
	if err != nil {
		return err
	}
	defer engine.Close()
	slog.Info("読み上げ暖機")
	japanese, err := engine.SynthesizeULaw("こんにちは")
	if err != nil {
		return fmt.Errorf("日本語の暖機失敗: %w", err)
	}
	english, err := engine.SynthesizeULaw("Hello.")
	if err != nil {
		return fmt.Errorf("英語の暖機失敗: %w", err)
	}
	if len(japanese) == 0 || len(english) == 0 {
		return fmt.Errorf("暖機失敗: 挨拶の音声が空です")
	}
	gap := bytes.Repeat([]byte{config.ULawSilence}, config.FrameSamples*8)
	greeting := append(append(japanese, gap...), english...)
	slog.Info("読み上げ暖機完了", "ja_bytes", len(japanese), "en_bytes", len(english))

	client := gemma.Client{
		ServerURL:    *server,
		Model:        *model,
		Instructions: *instructions,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("起動", "app", config.AppName, "server", client.ServerURL, "model", client.Model, "log", *logPath)
	application := app.New(ari.NewClient(creds), call.Options{
		DialResponder: func(ctx context.Context) (call.Responder, error) {
			return client.Dial(ctx)
		},
		Synth:            engine,
		GreetingULaw:     greeting,
		RTPBindHost:      config.RTPBindHost,
		FallbackPeerHost: config.ARIHost,
	})
	application.RunForever(ctx)
	slog.Info("停止")
	return nil
}
