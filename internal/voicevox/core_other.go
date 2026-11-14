//go:build !(linux && cgo)

package voicevox

import "errors"

// Load は Linux で cgo を有効にしたビルドでしか使えない。それ以外では常にエラーを返す。
func Load() (*Client, error) {
	return nil, errors.New("VOICEVOX CORE は linux かつ cgo 有効のビルドでだけ使える")
}
