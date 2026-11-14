//go:build linux && cgo

package voicevox

/*
#cgo CFLAGS: -DVOICEVOX_LOAD_ONNXRUNTIME
#cgo LDFLAGS: -lvoicevox_core
#include <stdlib.h>
#include <voicevox_core.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"unsafe"

	"denwa-sts/internal/config"
)

func resultError(code C.VoicevoxResultCode, what string) error {
	if code == C.VOICEVOX_RESULT_OK {
		return nil
	}
	return fmt.Errorf("%s: code=%d %s", what, int(code), C.GoString(C.voicevox_error_result_to_message(code)))
}

// cCore は VOICEVOX CORE の C API を呼ぶ core の実装。
type cCore struct {
	synthesizer *C.VoicevoxSynthesizer
	openJtalk   *C.OpenJtalkRc
}

func (c *cCore) CreateAudioQuery(text string, styleID uint32) (string, error) {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	var query *C.char
	code := C.voicevox_synthesizer_create_audio_query(c.synthesizer, cText, C.VoicevoxStyleId(styleID), &query)
	if err := resultError(code, "create_audio_query"); err != nil {
		return "", err
	}
	defer C.voicevox_string_free(query)
	return C.GoString(query), nil
}

func (c *cCore) Synthesis(audioQueryJSON string, styleID uint32) ([]byte, error) {
	cQuery := C.CString(audioQueryJSON)
	defer C.free(unsafe.Pointer(cQuery))

	var length C.uintptr_t
	var wav *C.uint8_t
	code := C.voicevox_synthesizer_synthesis(
		c.synthesizer, cQuery, C.VoicevoxStyleId(styleID),
		C.voicevox_make_default_synthesis_options(), &length, &wav,
	)
	if err := resultError(code, "synthesis"); err != nil {
		return nil, err
	}
	defer C.voicevox_wav_free(wav)
	return C.GoBytes(unsafe.Pointer(wav), C.int(length)), nil
}

func (c *cCore) Close() {
	if c.synthesizer != nil {
		C.voicevox_synthesizer_delete(c.synthesizer)
		c.synthesizer = nil
	}
	if c.openJtalk != nil {
		C.voicevox_open_jtalk_rc_delete(c.openJtalk)
		c.openJtalk = nil
	}
}

// Load は ONNX Runtime、辞書、音声モデルを読み込み、設定の話者で合成できる Client を返す。
func Load() (*Client, error) {
	libDir := filepath.Join(config.VoicevoxDir, "onnxruntime", "lib")
	recommended := C.GoString(C.voicevox_get_onnxruntime_lib_recommended_versioned_filename())
	ortPath, err := findOnnxRuntime(libDir, recommended)
	if err != nil {
		return nil, err
	}
	slog.Info("VOICEVOX読込", "ort", ortPath, "vvm", config.VoicevoxVVMPath)

	cOrtPath := C.CString(ortPath)
	defer C.free(unsafe.Pointer(cOrtPath))
	ortOptions := C.voicevox_make_default_load_onnxruntime_options()
	ortOptions.filename = cOrtPath
	var onnxruntime *C.VoicevoxOnnxruntime
	if err := resultError(C.voicevox_onnxruntime_load_once(ortOptions, &onnxruntime), "ONNX Runtime読込"); err != nil {
		return nil, err
	}

	c := &cCore{}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()

	cDictDir := C.CString(config.VoicevoxDictDir)
	defer C.free(unsafe.Pointer(cDictDir))
	if err := resultError(C.voicevox_open_jtalk_rc_new(cDictDir, &c.openJtalk), "OpenJTalk辞書読込"); err != nil {
		return nil, err
	}

	initOptions := C.voicevox_make_default_initialize_options()
	initOptions.acceleration_mode = C.VoicevoxAccelerationMode(C.VOICEVOX_ACCELERATION_MODE_CPU)
	initOptions.cpu_num_threads = C.uint16_t(config.VoicevoxCPUThreads)
	if err := resultError(C.voicevox_synthesizer_new(onnxruntime, c.openJtalk, initOptions, &c.synthesizer), "シンセサイザ作成"); err != nil {
		return nil, err
	}

	speakerID, err := loadVoiceModel(c)
	if err != nil {
		return nil, err
	}
	slog.Info("音声モデル読込完了", "speaker", config.VoicevoxSpeakerName, "id", speakerID)
	ok = true
	return newClient(c, speakerID), nil
}

func loadVoiceModel(c *cCore) (uint32, error) {
	cPath := C.CString(config.VoicevoxVVMPath)
	defer C.free(unsafe.Pointer(cPath))

	var model *C.VoicevoxVoiceModelFile
	if err := resultError(C.voicevox_voice_model_file_open(cPath, &model), "VVMを開く"); err != nil {
		return 0, err
	}
	defer C.voicevox_voice_model_file_delete(model)

	loadOptions := C.voicevox_make_default_load_voice_model_options()
	if err := resultError(C.voicevox_synthesizer_load_voice_model(c.synthesizer, model, loadOptions), "VVM読込"); err != nil {
		return 0, err
	}

	metas := C.voicevox_voice_model_file_create_metas_json(model)
	if metas == nil {
		return 0, errors.New("音声モデルのメタ情報を取得できない")
	}
	defer C.voicevox_string_free(metas)
	return pickSpeakerID(C.GoString(metas))
}
