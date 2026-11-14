// Package synth は日本語をずんだもん、英語を Piper で読み上げ、電話用の μ-law にする。
// 1つの文に両方の文字が混ざるときは、文字種の切れ目でつないで読む。
package synth

import (
	"strings"
	"unicode"

	"denwa-sts/internal/config"
	"denwa-sts/internal/piper"
	"denwa-sts/internal/voicevox"
)

const switchGapFrames = 4

const (
	scriptUnknown = iota
	scriptJA
	scriptEN
)

// engine は1言語分の読み上げ。
type engine interface {
	SynthesizeULaw(text string) ([]byte, error)
	Close()
}

// Client は日本語と英語の読み上げを持つ。
type Client struct {
	ja engine
	en engine
}

type speech struct {
	japanese bool
	text     string
}

// Load はずんだもんと Piper を起動する。
func Load() (*Client, error) {
	ja, err := voicevox.Load()
	if err != nil {
		return nil, err
	}
	en, err := piper.Load()
	if err != nil {
		ja.Close()
		return nil, err
	}
	return &Client{ja: ja, en: en}, nil
}

// Close は両方の読み上げを止める。
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.ja != nil {
		c.ja.Close()
	}
	if c.en != nil {
		c.en.Close()
	}
}

// SynthesizeULaw は text を読み上げた 8 kHz μ-law を返す。空白だけなら nil を返す。
func (c *Client) SynthesizeULaw(text string) ([]byte, error) {
	parts := splitSpeech(text)
	if len(parts) == 0 {
		return nil, nil
	}
	var out []byte
	for _, part := range parts {
		reader := c.en
		if part.japanese {
			reader = c.ja
		}
		audio, err := reader.SynthesizeULaw(part.text)
		if err != nil {
			return nil, err
		}
		if len(audio) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, switchGap()...)
		}
		out = append(out, audio...)
	}
	return out, nil
}

func switchGap() []byte {
	gap := make([]byte, config.FrameSamples*switchGapFrames)
	for i := range gap {
		gap[i] = config.ULawSilence
	}
	return gap
}

// splitSpeech は日本語のかたと英字のかたに分ける。数字や空白は、直前の言語に付ける。
func splitSpeech(text string) []speech {
	var (
		buf  []rune
		lang int
		out  []speech
	)
	flush := func() {
		spoken := strings.TrimSpace(string(buf))
		kind := lang
		buf = nil
		lang = scriptUnknown
		if spoken == "" || !hasSpeakable(spoken) {
			return
		}
		if kind == scriptUnknown {
			kind = scriptEN
			for _, r := range spoken {
				if r > unicode.MaxASCII {
					kind = scriptJA
					break
				}
			}
		}
		out = append(out, speech{japanese: kind == scriptJA, text: spoken})
	}
	for _, r := range text {
		next := scriptOf(r)
		if next != scriptUnknown && lang != scriptUnknown && next != lang {
			head, spaces := splitTrailingSpace(buf)
			buf = head
			flush()
			buf = append([]rune(nil), spaces...)
		}
		if next != scriptUnknown {
			lang = next
		}
		buf = append(buf, r)
	}
	flush()
	return out
}

func splitTrailingSpace(buf []rune) (head, spaces []rune) {
	i := len(buf)
	for i > 0 && unicode.IsSpace(buf[i-1]) {
		i--
	}
	return buf[:i], buf[i:]
}

func scriptOf(r rune) int {
	if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) || isJapaneseMark(r) {
		return scriptJA
	}
	if isLatinLetter(r) {
		return scriptEN
	}
	return scriptUnknown
}

func isJapaneseMark(r rune) bool {
	switch r {
	case '。', '、', '「', '」', '『', '』', '（', '）', '！', '？', '…', '・', 'ー', '々', '〆', '〜':
		return true
	default:
		return false
	}
}

func isLatinLetter(r rune) bool {
	if unicode.IsLetter(r) && unicode.Is(unicode.Latin, r) {
		return true
	}
	return (r >= 'Ａ' && r <= 'Ｚ') || (r >= 'ａ' && r <= 'ｚ')
}

func hasSpeakable(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
