// Package audio は PCM16 モノラル WAV の読み書きと、線形補間のリサンプルを行う。
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// ReadWAV は PCM16 モノラルの WAV を読む。
func ReadWAV(path string) ([]int16, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("WAVを読めない: %w", err)
	}
	return DecodeWAV(data)
}

// WriteWAV は PCM16 モノラルの WAV を書く。
func WriteWAV(path string, pcm []int16, sampleRate int) error {
	if sampleRate <= 0 {
		return errors.New("サンプルレートが不正です")
	}
	encoded := EncodeWAV(pcm, sampleRate)
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("WAVを書けない: %w", err)
	}
	return nil
}

// DecodeWAV は PCM16 モノラルの WAV バイト列をサンプルにする。
func DecodeWAV(data []byte) ([]int16, int, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, errors.New("WAVの形式ではない")
	}
	var sampleRate int
	var channels int
	var bits int
	var pcmBytes []byte
	offset := 12
	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size < 0 || offset+size > len(data) {
			return nil, 0, errors.New("WAVのチャンクが壊れている")
		}
		chunk := data[offset : offset+size]
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, 0, errors.New("WAVのfmtが短い")
			}
			format := binary.LittleEndian.Uint16(chunk[0:2])
			channels = int(binary.LittleEndian.Uint16(chunk[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			bits = int(binary.LittleEndian.Uint16(chunk[14:16]))
			if format != 1 {
				return nil, 0, errors.New("WAVはPCM16だけ読める")
			}
		case "data":
			pcmBytes = chunk
		}
		offset += size
		if size%2 == 1 {
			offset++
		}
	}
	if sampleRate <= 0 || channels != 1 || bits != 16 || pcmBytes == nil {
		return nil, 0, errors.New("WAVはPCM16モノラルである必要がある")
	}
	pcm, err := PCM16FromBytes(EvenBytes(pcmBytes))
	if err != nil {
		return nil, 0, err
	}
	return pcm, sampleRate, nil
}

// EncodeWAV は PCM16 モノラルの WAV バイト列を作る。
func EncodeWAV(pcm []int16, sampleRate int) []byte {
	pcmBytes := PCM16Bytes(pcm)
	dataSize := len(pcmBytes)
	// RIFF のサイズは、WAVE 以降の 36 バイトに data を足したもの。
	riffSize := 36 + dataSize
	out := make([]byte, 44+dataSize)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(riffSize))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataSize))
	copy(out[44:], pcmBytes)
	return out
}

// PCM16Bytes はリトルエンディアンの PCM16 にする。長さは常に偶数になる。
func PCM16Bytes(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

// PCM16FromBytes はリトルエンディアンの PCM16 をサンプルにする。
func PCM16FromBytes(b []byte) ([]int16, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("PCM16のバイト数が偶数ではない")
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out, nil
}

// EvenBytes は PCM16 の端数が残らないよう、末尾の 1 バイトを捨てる。
func EvenBytes(b []byte) []byte {
	if len(b)%2 == 1 {
		return b[:len(b)-1]
	}
	return b
}
