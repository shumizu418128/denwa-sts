// Package voicevox は VOICEVOX CORE で、ずんだもんの音声を μ-law にする。
//
// C API を呼ぶ部分 (core_cgo.go) と、それ以外 (このファイル) に分けてある。
package voicevox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"denwa-sts/internal/audio"
	"denwa-sts/internal/config"
)

// core は VOICEVOX CORE の最小限の操作。
type core interface {
	CreateAudioQuery(text string, styleID uint32) (string, error)
	Synthesis(audioQueryJSON string, styleID uint32) ([]byte, error)
	Close()
}

// Client は読み上げ用の合成器。合成は同時に1つだけ走らせる (通話が重なってもロックで直列化する)。
type Client struct {
	core      core
	speakerID uint32
	mu        sync.Mutex
}

func newClient(c core, speakerID uint32) *Client {
	return &Client{core: c, speakerID: speakerID}
}

// SpeakerID は使っているスタイルの ID を返す。
func (c *Client) SpeakerID() uint32 { return c.speakerID }

// Close は合成器を解放する。
func (c *Client) Close() { c.core.Close() }

// SynthesizeULaw は text を読み上げた 8 kHz μ-law を返す。空白だけなら nil を返す。
func (c *Client) SynthesizeULaw(text string) ([]byte, error) {
	spoken := strings.Join(strings.Fields(text), "")
	if spoken == "" {
		return nil, nil
	}
	wav, err := c.synthesizeWav(spoken)
	if err != nil {
		return nil, err
	}
	return audio.WavToUlaw(wav)
}

func (c *Client) synthesizeWav(spoken string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	query, err := c.core.CreateAudioQuery(spoken, c.speakerID)
	if err != nil {
		return nil, fmt.Errorf("音声クエリ作成失敗: %w", err)
	}
	query, err = withVolume(query, config.VoicevoxVolume)
	if err != nil {
		return nil, err
	}
	wav, err := c.core.Synthesis(query, c.speakerID)
	if err != nil {
		return nil, fmt.Errorf("音声合成失敗: %w", err)
	}
	return wav, nil
}

// withVolume は音声クエリの JSON の volumeScale を差し替える。ほかの項目はそのまま残す。
func withVolume(queryJSON string, volume float64) (string, error) {
	var query map[string]any
	if err := json.Unmarshal([]byte(queryJSON), &query); err != nil {
		return "", fmt.Errorf("音声クエリを解析できない: %w", err)
	}
	query["volumeScale"] = volume
	out, err := json.Marshal(query)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// speakerMeta は VOICEVOX のメタ情報 (声の一覧) の必要な項目。
type speakerMeta struct {
	Name   string `json:"name"`
	Styles []struct {
		Name string `json:"name"`
		ID   uint32 `json:"id"`
	} `json:"styles"`
}

// pickSpeakerID は設定した話者のノーマル、なければ先頭スタイルの ID を返す。
func pickSpeakerID(metasJSON string) (uint32, error) {
	var metas []speakerMeta
	if err := json.Unmarshal([]byte(metasJSON), &metas); err != nil {
		return 0, fmt.Errorf("メタ情報を解析できない: %w", err)
	}
	for _, speaker := range metas {
		if speaker.Name != config.VoicevoxSpeakerName || len(speaker.Styles) == 0 {
			continue
		}
		for _, style := range speaker.Styles {
			if style.Name == config.VoicevoxStyleName {
				return style.ID, nil
			}
		}
		return speaker.Styles[0].ID, nil
	}
	return 0, fmt.Errorf("音声モデルに%sがいない", config.VoicevoxSpeakerName)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// findOnnxRuntime は libDir から ONNX Runtime の共有ライブラリを探す。
// 推奨ファイル名があればそれを、なければ番号付きの最後のものを返す。
func findOnnxRuntime(libDir, recommended string) (string, error) {
	preferred := filepath.Join(libDir, recommended)
	if fileExists(preferred) {
		return preferred, nil
	}
	matches, err := filepath.Glob(filepath.Join(libDir, "libvoicevox_onnxruntime.so*"))
	if err != nil {
		return "", err
	}
	var found []string
	for _, path := range matches {
		if strings.Contains(filepath.Base(path), ".so.") {
			found = append(found, path)
		}
	}
	if len(found) == 0 {
		return "", errors.New("ONNX Runtime がない: " + libDir)
	}
	sort.Strings(found)
	return found[len(found)-1], nil
}
