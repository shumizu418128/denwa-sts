package lfm

import (
	"strings"
	"unicode"
)

const (
	// weakMinRunes は読点で区切って読み始めるために必要な最小文字数。
	weakMinRunes = 12
	// hardMaxRunes は句読点が無いときに強制的に区切る文字数。
	hardMaxRunes = 32
)

// segmenter はモデルの差分テキストを、読み上げを始められる断片に分ける。
// 文末がバッファの終端にあるあいだは、閉じ括弧が続くかもしれないので保留する。
type segmenter struct {
	buf []rune
}

func (s *segmenter) Push(delta string) []string {
	if delta == "" {
		return nil
	}
	s.buf = append(s.buf, []rune(delta)...)
	return s.drain(false)
}

func (s *segmenter) Flush() []string {
	return s.drain(true)
}

func (s *segmenter) drain(final bool) []string {
	var out []string
	for {
		cut := nextCut(s.buf, final)
		if cut <= 0 {
			return out
		}
		if cut > len(s.buf) {
			cut = len(s.buf)
		}
		chunk := strings.TrimSpace(string(s.buf[:cut]))
		rest := make([]rune, len(s.buf)-cut)
		copy(rest, s.buf[cut:])
		s.buf = trimLeadingSpace(rest)
		if chunk != "" {
			out = append(out, chunk)
		}
	}
}

func trimLeadingSpace(buf []rune) []rune {
	i := 0
	for i < len(buf) && unicode.IsSpace(buf[i]) {
		i++
	}
	if i == 0 {
		return buf
	}
	if i == len(buf) {
		return nil
	}
	return buf[i:]
}

func nextCut(buf []rune, final bool) int {
	if speakableRunes(buf) == 0 {
		return 0
	}
	lastWeak := 0
	for i := range buf {
		strong := isStrongBreak(buf, i)
		weak := isWeakBreak(buf, i)
		if !strong && !weak {
			continue
		}
		end := absorbClosers(buf, i+1)
		if speakableRunes(buf[:end]) == 0 {
			continue
		}
		if weak && speakableRunes(buf[:end]) < weakMinRunes {
			if end <= hardMaxRunes {
				lastWeak = end
			}
			continue
		}
		// 文末が終端なら閉じ括弧を待つ。短い文は生成完了までまとめる。
		if strong && end == len(buf) && !final && end <= hardMaxRunes {
			return 0
		}
		if end > hardMaxRunes {
			return forcedCut(buf, lastWeak)
		}
		return end
	}
	if len(buf) >= hardMaxRunes {
		return forcedCut(buf, lastWeak)
	}
	if final {
		return len(buf)
	}
	return 0
}

func forcedCut(buf []rune, lastWeak int) int {
	if lastWeak >= 4 && lastWeak <= hardMaxRunes && speakableRunes(buf[:lastWeak]) >= 4 {
		return lastWeak
	}
	cut := hardMaxRunes
	if cut > len(buf) {
		cut = len(buf)
	}
	// 英語は空白の位置で切り、単語の途中で読ませない。
	if space := lastBreakSpace(buf[:cut]); space >= 4 {
		return space
	}
	return cut
}

func lastBreakSpace(buf []rune) int {
	for i := len(buf) - 1; i >= 0; i-- {
		if unicode.IsSpace(buf[i]) {
			return i
		}
	}
	return 0
}

func absorbClosers(buf []rune, i int) int {
	for i < len(buf) && isCloser(buf[i]) {
		i++
	}
	return i
}

func speakableRunes(buf []rune) int {
	n := 0
	for _, r := range buf {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func isStrongBreak(buf []rune, i int) bool {
	switch buf[i] {
	case '。', '！', '？', '!', '?', '…', '．', '\n', '\r':
		return true
	case '.':
		// 小数点や example.com のドットでは切らない。文末のピリオドは次の文字が来るまで保留する。
		if i > 0 && isDigit(buf[i-1]) && (i+1 == len(buf) || isDigit(buf[i+1])) {
			return false
		}
		if i+1 == len(buf) {
			return true
		}
		next := buf[i+1]
		return unicode.IsSpace(next) || isCloser(next)
	default:
		return false
	}
}

func isWeakBreak(buf []rune, i int) bool {
	switch buf[i] {
	case '、', '，', '；', ';', '：', ':':
		return true
	case ',':
		if i > 0 && isDigit(buf[i-1]) && (i+1 == len(buf) || isDigit(buf[i+1])) {
			return false
		}
		return true
	default:
		return false
	}
}

func isCloser(r rune) bool {
	switch r {
	case '」', '』', '）', ')', '】', '〉', '》', '〕', '〗', '"', '\'':
		return true
	default:
		return false
	}
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
