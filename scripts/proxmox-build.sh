#!/usr/bin/env bash
# Proxmox (ユーザー denwa) 上で denwa-sts をビルドし、再起動する。
# 通常は Windows から deploy-proxmox.ps1 経由で実行する。
set -euo pipefail

ROOT="${DENWA_STS_BUILD_DIR:-/tmp/denwa-sts-build}"
STS_DIR="/home/denwa/sts"

export PATH="${HOME}/.local/go/bin:${PATH}"
export CGO_ENABLED=1
export CGO_CFLAGS="-I${HOME}/voicevox/core/include"
export CGO_LDFLAGS="-L${HOME}/voicevox/core/lib -Wl,-rpath,${HOME}/voicevox/core/lib"

bash "${ROOT}/scripts/setup-piper.sh"

cd "${ROOT}"
go test ./...
go build -o denwa-sts ./cmd/denwa-sts
install -m 755 denwa-sts "${STS_DIR}/denwa-sts"

if pkill -x denwa-sts 2>/dev/null; then
	sleep 1
fi

cd "${STS_DIR}"
nohup ./denwa-sts >>denwa_sts.out 2>&1 &
sleep 1

if ! pgrep -x denwa-sts >/dev/null; then
	echo "denwa-sts の起動に失敗しました。tail -f ${STS_DIR}/denwa_sts.out を確認してください。" >&2
	exit 1
fi

pgrep -a denwa-sts
tail -n 8 denwa_sts.log 2>/dev/null || true