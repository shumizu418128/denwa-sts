package segment

import (
	"strings"
	"testing"
)

func feed(deltas ...string) []string {
	var seg Segmenter
	var out []string
	for _, delta := range deltas {
		out = append(out, seg.Push(delta)...)
	}
	out = append(out, seg.Flush()...)
	return out
}

func TestSegmenterSplitsOnJapaneseSentenceEnd(t *testing.T) {
	var seg Segmenter
	if parts := seg.Push("こんにちは。"); len(parts) != 1 || parts[0] != "こんにちは。" {
		t.Fatalf("句点で断片が出ていない: %q", parts)
	}
	parts := seg.Push("明日は晴れです。")
	if len(parts) != 1 || parts[0] != "明日は晴れです。" {
		t.Fatalf("parts=%q", parts)
	}
	if rest := seg.Flush(); len(rest) != 0 {
		t.Fatalf("flush=%q", rest)
	}
}

func TestSegmenterHoldsClosingQuote(t *testing.T) {
	var seg Segmenter
	if parts := seg.Push("「はい。"); len(parts) != 0 {
		t.Fatalf("閉じ括弧を待たずに出した: %q", parts)
	}
	parts := seg.Push("」わかった。")
	if len(parts) != 2 || parts[0] != "「はい。」" || parts[1] != "わかった。" {
		t.Fatalf("parts=%q", parts)
	}
	if rest := seg.Flush(); len(rest) != 0 {
		t.Fatalf("flush=%q", rest)
	}
}

func TestSegmenterKeepsThousandsSeparator(t *testing.T) {
	got := feed("合計は1,000円です。")
	if len(got) != 1 || got[0] != "合計は1,000円です。" {
		t.Fatalf("got=%q", got)
	}
}

func TestSegmenterSplitsLongClauseAtComma(t *testing.T) {
	head := strings.Repeat("あ", WeakMinRunes)
	var seg Segmenter
	if parts := seg.Push(head + "、"); len(parts) != 1 || parts[0] != head+"、" {
		t.Fatalf("parts=%q", parts)
	}
	rest := seg.Flush()
	if len(rest) != 0 {
		t.Fatalf("flush=%q", rest)
	}
}

func TestSegmenterForcesCutWithoutPunctuation(t *testing.T) {
	var seg Segmenter
	body := strings.Repeat("あ", HardMaxRunes+8)
	parts := seg.Push(body)
	if len(parts) != 1 || len([]rune(parts[0])) != HardMaxRunes {
		t.Fatalf("parts=%q", parts)
	}
	rest := seg.Flush()
	if len(rest) != 1 || len([]rune(rest[0])) != 8 {
		t.Fatalf("flush=%q", rest)
	}
}

func TestSegmenterFlushKeepsShortReply(t *testing.T) {
	got := feed("もしもし")
	if len(got) != 1 || got[0] != "もしもし" {
		t.Fatalf("got=%q", got)
	}
}

func TestSegmenterSplitsEnglishSentenceAfterNextRune(t *testing.T) {
	var seg Segmenter
	if parts := seg.Push("Hello."); len(parts) != 0 {
		t.Fatalf("文末で確定していない: %q", parts)
	}
	parts := seg.Push(" How are you?")
	if len(parts) != 2 || parts[0] != "Hello." || parts[1] != "How are you?" {
		t.Fatalf("parts=%q", parts)
	}
	if rest := seg.Flush(); len(rest) != 0 {
		t.Fatalf("flush=%q", rest)
	}
}

func TestSegmenterKeepsDecimalPoint(t *testing.T) {
	got := feed("It costs 3.14 dollars.")
	if len(got) != 1 || got[0] != "It costs 3.14 dollars." {
		t.Fatalf("got=%q", got)
	}
}

func TestSegmenterKeepsDomainDot(t *testing.T) {
	got := feed("See example.com today.")
	if len(got) != 1 || got[0] != "See example.com today." {
		t.Fatalf("got=%q", got)
	}
}

func TestSegmenterCutsEnglishOnWordBoundary(t *testing.T) {
	body := "Please hold while I look up your account details right now"
	if len([]rune(body)) <= HardMaxRunes {
		t.Fatalf("len=%d", len([]rune(body)))
	}
	got := feed(body)
	if len(got) < 2 {
		t.Fatalf("got=%q", got)
	}
	if strings.Join(got, " ") != body {
		t.Fatalf("joined=%q parts=%q", strings.Join(got, " "), got)
	}
}
