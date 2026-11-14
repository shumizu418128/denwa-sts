// Command sts は WAV の 1 往復を llama-server の /v1/realtime に送る。
//
// llama-server は別プロセスで動かす。GPU は要らない。
//
//	llama-server -hf LiquidAI/LFM3-Omni-2B-GGUF:Q4_K_M --reasoning off
//
//	go run ./cmd/sts -in question.wav -out answer.wav
//
// 待ち受けの既定は ws://127.0.0.1:8080/v1/realtime
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"denwa-sts/internal/audio"
	"denwa-sts/internal/lfm"
)

func main() {
	if err := run(); err != nil {
		slog.Error("異常終了", "error", err)
		os.Exit(1)
	}
}

func run() error {
	server := flag.String("server", lfm.DefaultServerURL, "llama-server の /v1/realtime")
	inPath := flag.String("in", "", "入力 WAV（PCM16 モノラル）")
	outPath := flag.String("out", "", "出力 WAV")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使い方: sts -in question.wav -out answer.wav [-server %s]\n", lfm.DefaultServerURL)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *inPath == "" || *outPath == "" {
		return errors.New("-in と -out が必要です")
	}

	pcm, rate, err := audio.ReadWAV(*inPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reply, err := (lfm.Client{ServerURL: *server}).Turn(ctx, pcm, rate)
	if err != nil {
		return err
	}
	slog.Info("返事を書く", "path", *outPath, "rate", reply.SampleRate, "text", reply.Text)
	return audio.WriteWAV(*outPath, reply.PCM, reply.SampleRate)
}
