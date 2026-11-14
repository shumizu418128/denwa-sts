// Package config は denwa-sts の設定値と環境設定の読込をまとめる。
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// --- Asterisk ARI ---
const (
	ARIHost    = "10.10.10.10"
	ARIAddr    = ARIHost + ":8088"
	AppName    = "denwa_sts"
	EnvPath    = "/home/denwa/ari.env"
	ARIBaseURL = "http://" + ARIAddr + "/asterisk/ari"
	ARIWSURL   = "ws://" + ARIAddr + "/asterisk/ari/events"
)

// --- RTP ---
const (
	RTPBindHost   = "10.10.10.1"
	SampleRate    = 8000
	FrameSamples  = 160
	FrameDuration = 20 * time.Millisecond
	PayloadPCMU   = 0
	ULawSilence   = 0xFF
)

// --- VOICEVOX (日本語の読み上げ) ---
const (
	VoicevoxDir         = "/home/denwa/voicevox"
	VoicevoxVVMPath     = VoicevoxDir + "/0.vvm"
	VoicevoxDictDir     = VoicevoxDir + "/dict/open_jtalk_dic_utf_8-1.11"
	VoicevoxSpeakerName = "ずんだもん"
	VoicevoxStyleName   = "ノーマル"
	VoicevoxVolume      = 1.0
	VoicevoxCPUThreads  = 2
)

// --- Piper (英語の読み上げ) ---
const (
	PiperDir        = "/home/denwa/piper"
	PiperPython     = PiperDir + "/.venv/bin/python"
	PiperModelPath  = PiperDir + "/en_US-lessac-medium.onnx"
	PiperCPUThreads = 2
)

// --- ログ ---
const DefaultLogPath = "/home/denwa/sts/denwa_sts.log"

// SetupLogging は slog の既定ロガーを標準出力と logPath（空でなければファイル）へ向ける。
func SetupLogging(logPath string) error {
	writers := []io.Writer{os.Stdout}
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return fmt.Errorf("ログディレクトリ作成失敗: %w", err)
		}
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("ログファイルを開けない: %w", err)
		}
		writers = append(writers, file)
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}
