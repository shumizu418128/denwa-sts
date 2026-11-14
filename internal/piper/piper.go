// Package piper は Piper で英語の文章を電話用の 8 kHz μ-law にする。
//
// モデルの読み込みが重いので、Python のワーカーを常駐させて文ごとに合成する。
package piper

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"denwa-sts/internal/audio"
	"denwa-sts/internal/config"

	_ "embed"
)

//go:embed worker.py
var workerSource string

const (
	readyTimeout = 90 * time.Second
	stopTimeout  = 2 * time.Second
	maxTextBytes = 8000
	maxPCMBytes  = 8 << 20
)

var errFraming = errors.New("読み上げの応答が壊れました")

// Client は常駐ワーカーへ合成を頼む。合成は同時に1つだけ走らせる。
type Client struct {
	python  string
	script  string
	model   string
	threads int

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	done   chan struct{}
	logs   *logBuf
}

// Load は設定の Python と音声モデルでワーカーを起動する。
func Load() (*Client, error) {
	script, err := materializeWorker()
	if err != nil {
		return nil, err
	}
	python, err := resolvePython(config.PiperPython)
	if err != nil {
		return nil, err
	}
	client, err := start(python, script, config.PiperModelPath, config.PiperCPUThreads)
	if err != nil {
		return nil, err
	}
	slog.Info("Piper読込完了", "model", client.model, "python", client.python)
	return client, nil
}

func start(python, script, model string, threads int) (*Client, error) {
	if !regularFile(model) || !regularFile(model+".json") {
		return nil, fmt.Errorf("音声モデルがない: %s （scripts/setup-piper.sh を実行してください）", model)
	}
	client := &Client{
		python:  python,
		script:  script,
		model:   model,
		threads: threads,
	}
	if err := client.startLocked(); err != nil {
		return nil, err
	}
	return client, nil
}

// Close はワーカーを止める。
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// SynthesizeULaw は text を読み上げた 8 kHz μ-law を返す。空白だけなら nil を返す。
func (c *Client) SynthesizeULaw(text string) ([]byte, error) {
	spoken := strings.Join(strings.Fields(text), " ")
	if spoken == "" {
		return nil, nil
	}
	if len(spoken) > maxTextBytes {
		return nil, errors.New("文章が長すぎます")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	pcm, rate, err := c.synthesizeLocked(spoken)
	if err != nil {
		return nil, err
	}
	if len(pcm) == 0 {
		return nil, nil
	}
	return audio.PCM16SamplesToULaw(pcm, rate)
}

func (c *Client) synthesizeLocked(text string) ([]int16, int, error) {
	if err := c.ensureLocked(); err != nil {
		return nil, 0, err
	}
	pcm, rate, err := c.roundTripLocked(text)
	if err == nil || !isIOError(err) {
		return pcm, rate, err
	}
	c.stopLocked()
	if err := c.startLocked(); err != nil {
		return nil, 0, err
	}
	return c.roundTripLocked(text)
}

func (c *Client) ensureLocked() error {
	c.reapIfExitedLocked()
	if c.cmd != nil {
		return nil
	}
	return c.startLocked()
}

func (c *Client) startLocked() error {
	cmd := exec.Command(c.python, "-u", c.script, "--model", c.model, "--threads", fmt.Sprintf("%d", c.threads))
	cmd.Env = append(os.Environ(), "ORT_DISABLE_TELEMETRY=1", "PYTHONUNBUFFERED=1", "PYTHONSAFEPATH=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("読み上げを起動できない: %w", err)
	}

	done := make(chan struct{})
	logs := &logBuf{}
	c.cmd = cmd
	c.stdin = stdin
	c.stdout = bufio.NewReader(stdout)
	c.done = done
	c.logs = logs

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			logs.add(line)
			slog.Debug("Piper", "msg", line)
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	if err := c.readReady(); err != nil {
		c.stopLocked()
		select {
		case <-stderrDone:
		case <-time.After(stopTimeout):
		}
		detail := logs.String()
		if detail == "" {
			return fmt.Errorf("読み上げを起動できない: %w", err)
		}
		return fmt.Errorf("読み上げを起動できない: %w: %s", err, detail)
	}
	return nil
}

func (c *Client) readReady() error {
	ch := make(chan error, 1)
	go func() {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			ch <- err
			return
		}
		if strings.TrimSpace(line) != "READY" {
			ch <- fmt.Errorf("起動応答が不正です: %s", strings.TrimSpace(line))
			return
		}
		ch <- nil
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(readyTimeout):
		return errors.New("起動がタイムアウトしました")
	}
}

func (c *Client) roundTripLocked(text string) ([]int16, int, error) {
	if err := writeRequest(c.stdin, text); err != nil {
		return nil, 0, err
	}
	pcm, rate, err := readResponse(c.stdout)
	if err != nil {
		return nil, 0, err
	}
	if len(pcm)%2 != 0 {
		return nil, 0, errFraming
	}
	samples, err := audio.PCM16FromBytes(pcm)
	if err != nil {
		return nil, 0, err
	}
	return samples, rate, nil
}

func (c *Client) reapIfExitedLocked() {
	if c.cmd == nil || c.done == nil {
		return
	}
	select {
	case <-c.done:
		if c.stdin != nil {
			_ = c.stdin.Close()
			c.stdin = nil
		}
		c.cmd = nil
		c.stdout = nil
	default:
	}
}

func (c *Client) stopLocked() {
	if c.cmd == nil {
		return
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	done := c.done
	cmd := c.cmd
	select {
	case <-done:
	case <-time.After(stopTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
	c.cmd = nil
	c.stdout = nil
}

func writeRequest(w io.Writer, text string) error {
	payload := []byte(text)
	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(payload)))
	copy(buf[4:], payload)
	return writeAll(w, buf)
}

func readResponse(r io.Reader) ([]byte, int, error) {
	var status uint32
	if err := binary.Read(r, binary.LittleEndian, &status); err != nil {
		return nil, 0, err
	}
	if status != 0 {
		msg, err := readBlock(r)
		if err != nil {
			return nil, 0, err
		}
		text := strings.TrimSpace(string(msg))
		if text == "" {
			text = "音声合成失敗"
		}
		return nil, 0, errors.New(text)
	}
	var rate uint32
	if err := binary.Read(r, binary.LittleEndian, &rate); err != nil {
		return nil, 0, err
	}
	if rate == 0 {
		return nil, 0, errFraming
	}
	pcm, err := readBlock(r)
	if err != nil {
		return nil, 0, err
	}
	return pcm, int(rate), nil
}

func readBlock(r io.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > maxPCMBytes {
		return nil, errFraming
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeAll(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func isIOError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, errFraming) {
		return true
	}
	// exec のパイプが切れたときは PathError になる。ワーカーの合成エラーはこれに当てはまらない。
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}

func materializeWorker() (string, error) {
	path := filepath.Join(os.TempDir(), "denwa-sts-piper-worker.py")
	if err := os.WriteFile(path, []byte(workerSource), 0o644); err != nil {
		return "", fmt.Errorf("読み上げスクリプトを書けない: %w", err)
	}
	return path, nil
}

func resolvePython(configured string) (string, error) {
	if configured != "" {
		if info, err := os.Stat(configured); err == nil && !info.IsDir() {
			return configured, nil
		}
	}
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	if configured != "" {
		return "", fmt.Errorf("python が見つからない: %s", configured)
	}
	return "", errors.New("python が見つからない")
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > 20 {
		l.lines = l.lines[len(l.lines)-20:]
	}
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
