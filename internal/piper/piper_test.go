package piper

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResponseFrame(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, uint32(0)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(22050)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(pcm))); err != nil {
		t.Fatal(err)
	}
	buf.Write(pcm)

	got, rate, err := readResponse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 22050 || !bytes.Equal(got, pcm) {
		t.Fatalf("rate=%d pcm=%v", rate, got)
	}
}

func TestResponseError(t *testing.T) {
	msg := []byte("合成できない")
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, uint32(1)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(msg))); err != nil {
		t.Fatal(err)
	}
	buf.Write(msg)

	_, _, err := readResponse(&buf)
	if err == nil || !strings.Contains(err.Error(), "合成できない") {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteRequest(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRequest(&buf, "Hi"); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(buf.Bytes()[:4]) != 2 || string(buf.Bytes()[4:]) != "Hi" {
		t.Fatalf("bytes=%v", buf.Bytes())
	}
}

func TestSynthesizeULawSkipsBlank(t *testing.T) {
	out, err := (&Client{}).SynthesizeULaw("  \n\t ")
	if err != nil || out != nil {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestSynthesizeULawRejectsHugeText(t *testing.T) {
	_, err := (&Client{}).SynthesizeULaw(strings.Repeat("a", maxTextBytes+1))
	if err == nil {
		t.Fatal("長い文章を拒否していない")
	}
}

func TestClientSynthesizesWithFakeWorker(t *testing.T) {
	python := findPython()
	if python == "" {
		t.Skip("python がない")
	}
	dir := t.TempDir()
	model := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model+".json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake.py")
	if err := os.WriteFile(script, []byte(fakeWorker), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := start(python, script, model, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	first, err := client.SynthesizeULaw("Hello there")
	if err != nil || len(first) == 0 {
		t.Fatalf("first=%d err=%v", len(first), err)
	}
	second, err := client.SynthesizeULaw("Hello.")
	if err != nil || len(second) == 0 {
		t.Fatalf("second=%d err=%v", len(second), err)
	}
	if _, err := client.SynthesizeULaw("boom"); err == nil || !strings.Contains(err.Error(), "合成できない") {
		t.Fatalf("err=%v", err)
	}
}

func TestWorkerScriptCompiles(t *testing.T) {
	python := findPython()
	if python == "" {
		t.Skip("python がない")
	}
	script, err := materializeWorker()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-m", "py_compile", script).CombinedOutput()
	if err != nil {
		t.Fatalf("py_compile: %v\n%s", err, out)
	}
}

func findPython() string {
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

const fakeWorker = `
import struct
import sys

def read_exact(n):
    buf = b""
    while len(buf) < n:
        chunk = sys.stdin.buffer.read(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf

sys.stdout.buffer.write(b"READY\n")
sys.stdout.buffer.flush()

while True:
    header = read_exact(4)
    if header is None:
        break
    (n,) = struct.unpack("<I", header)
    if n == 0:
        break
    text = read_exact(n)
    if text is None:
        break
    if text == b"boom":
        msg = "合成できない".encode()
        sys.stdout.buffer.write(struct.pack("<II", 1, len(msg)))
        sys.stdout.buffer.write(msg)
        sys.stdout.buffer.flush()
        continue
    if b" " not in text and text != b"Hello.":
        msg = "space missing".encode()
        sys.stdout.buffer.write(struct.pack("<II", 1, len(msg)))
        sys.stdout.buffer.write(msg)
        sys.stdout.buffer.flush()
        continue
    pcm = struct.pack("<160h", *([1000] * 160))
    sys.stdout.buffer.write(struct.pack("<III", 0, 22050, len(pcm)))
    sys.stdout.buffer.write(pcm)
    sys.stdout.buffer.flush()
`
