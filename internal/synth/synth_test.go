package synth

import (
	"strings"
	"testing"

	"denwa-sts/internal/config"
)

type memEngine struct {
	tag  byte
	got  []string
	fail error
}

func (m *memEngine) SynthesizeULaw(text string) ([]byte, error) {
	m.got = append(m.got, text)
	if m.fail != nil {
		return nil, m.fail
	}
	return []byte{m.tag}, nil
}

func (m *memEngine) Close() {}

func TestSplitSpeechRoutesLanguages(t *testing.T) {
	cases := []struct {
		in   string
		want []speech
	}{
		{in: "こんにちは。", want: []speech{{japanese: true, text: "こんにちは。"}}},
		{in: "Hello there.", want: []speech{{japanese: false, text: "Hello there."}}},
		{in: "Hello. 明日は晴れです。", want: []speech{
			{japanese: false, text: "Hello."},
			{japanese: true, text: "明日は晴れです。"},
		}},
		{in: "OKです", want: []speech{
			{japanese: false, text: "OK"},
			{japanese: true, text: "です"},
		}},
		{in: "300円です", want: []speech{{japanese: true, text: "300円です"}}},
		{in: "3.14 dollars", want: []speech{{japanese: false, text: "3.14 dollars"}}},
		{in: "See example.com today.", want: []speech{{japanese: false, text: "See example.com today."}}},
		{in: "   ", want: nil},
		{in: "、。", want: nil},
	}
	for _, tc := range cases {
		got := splitSpeech(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%q got=%+v want=%+v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q got=%+v want=%+v", tc.in, got, tc.want)
			}
		}
	}
}

func TestSynthesizeULawJoinsBothLanguages(t *testing.T) {
	ja := &memEngine{tag: 'J'}
	en := &memEngine{tag: 'E'}
	client := &Client{ja: ja, en: en}

	got, err := client.SynthesizeULaw("Hello. 明日は晴れです。")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(en.got, "|") != "Hello." || strings.Join(ja.got, "|") != "明日は晴れです。" {
		t.Fatalf("en=%q ja=%q", en.got, ja.got)
	}
	want := config.FrameSamples*switchGapFrames + 2
	if len(got) != want || got[0] != 'E' || got[len(got)-1] != 'J' {
		t.Fatalf("len=%d first=%q last=%q", len(got), got[0], got[len(got)-1])
	}
}

func TestSynthesizeULawSkipsBlank(t *testing.T) {
	ja := &memEngine{tag: 'J'}
	en := &memEngine{tag: 'E'}
	got, err := (&Client{ja: ja, en: en}).SynthesizeULaw("  \n")
	if err != nil || got != nil || len(ja.got) != 0 || len(en.got) != 0 {
		t.Fatalf("got=%v err=%v ja=%v en=%v", got, err, ja.got, en.got)
	}
}
