#!/usr/bin/env bash
# Piper の仮想環境と英語音声を $HOME/piper に用意する。
# Debian の python3 には pip が無いことがあるため、パッケージは uv で入れる。
set -euo pipefail

PIPER_DIR="${HOME}/piper"
VENV="${PIPER_DIR}/.venv"
VOICE="en_US-lessac-medium"
MODEL="${PIPER_DIR}/${VOICE}.onnx"
PY="${VENV}/bin/python"

export PATH="${HOME}/.local/bin:${PATH}"
# データ用の $HOME/piper が、ホームで起動した python の piper パッケージに見えないようにする。
export PYTHONSAFEPATH=1

if ! command -v python3 >/dev/null 2>&1; then
	echo "python3 がありません" >&2
	exit 1
fi

if ! command -v uv >/dev/null 2>&1; then
	curl -LsSf https://astral.sh/uv/install.sh | sh
	export PATH="${HOME}/.local/bin:${PATH}"
fi

if ! command -v uv >/dev/null 2>&1; then
	echo "uv を入れられませんでした" >&2
	exit 1
fi

mkdir -p "${PIPER_DIR}"

# 前回の python3 -m venv は pip 無しで残ることがある。インタープリタが動かなければ作り直す。
if [[ ! -x "${PY}" ]] || ! "${PY}" -c "import sys" >/dev/null 2>&1; then
	rm -rf "${VENV}"
	uv venv --python python3 "${VENV}"
fi

if ! "${PY}" -c "from piper import PiperVoice" >/dev/null 2>&1; then
	uv pip install --python "${PY}" piper-tts
fi

if [[ ! -f "${MODEL}" || ! -f "${MODEL}.json" ]]; then
	"${PY}" -m piper.download_voices "${VOICE}" --data-dir "${PIPER_DIR}"
fi

test -f "${MODEL}"
test -f "${MODEL}.json"
echo "Piper準備完了: ${MODEL}"
