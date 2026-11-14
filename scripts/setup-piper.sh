#!/usr/bin/env bash
# Piper の仮想環境と英語音声を $HOME/piper に用意する。
# denwa ユーザーのまま入れる。システム Python は汚さない。
set -euo pipefail

PIPER_DIR="${HOME}/piper"
VENV="${PIPER_DIR}/.venv"
VOICE="en_US-lessac-medium"
MODEL="${PIPER_DIR}/${VOICE}.onnx"

mkdir -p "${PIPER_DIR}"

if [[ ! -x "${VENV}/bin/python" ]]; then
	if command -v uv >/dev/null 2>&1; then
		uv venv "${VENV}"
	else
		python3 -m venv "${VENV}"
	fi
fi

PY="${VENV}/bin/python"

if ! "${PY}" -c "import piper" >/dev/null 2>&1; then
	if command -v uv >/dev/null 2>&1; then
		uv pip install --python "${PY}" piper-tts
	else
		"${PY}" -m pip install piper-tts
	fi
fi

if [[ ! -f "${MODEL}" || ! -f "${MODEL}.json" ]]; then
	"${PY}" -m piper.download_voices "${VOICE}" --data-dir "${PIPER_DIR}"
fi

test -f "${MODEL}"
test -f "${MODEL}.json"
echo "Piper準備完了: ${MODEL}"
