# Proxmox への反映

修正後、電話用バイナリ `denwa-sts` を Proxmox ホスト上の `/home/denwa/sts/denwa-sts` に載せ替える手順。

- SSH: `~/.ssh/config` のホスト名 `proxmox`（`denwa@192.168.11.40`）
- 日本語は VOICEVOX のずんだもん。CGO は Proxmox 上の `/home/denwa/voicevox/` を参照する
- 英語は Piper（`en_US-lessac-medium`）。モデルと仮想環境は `/home/denwa/piper/`
- 返答の文字種で読み上げを切り替える
- `llama-server` は別プロセスのまま。ここでは `denwa-sts` だけ更新する
- モデルは **Gemma 4 E4B**（音声入力 → テキスト）。`denwa-sts` は `/v1/chat/completions` に `input_audio` を送る

## llama-server（Gemma 4 E4B）

LFM3 の `/v1/realtime` はやめ、音声対応の Gemma を chat completions で待つ。

```bash
# 例: ggml-org の GGUF（このリポは Q4_0 / Q8_0。Q4_K_M は無い）
# CPU なら -ngl 0、-t はコア数に合わせる。mmproj は -hf が一緒に取る
/home/denwa/sts/llama-b11960/llama-server \
  -hf ggml-org/gemma-4-E4B-it-GGUF:Q4_0 \
  --host 127.0.0.1 --port 8080 \
  -c 8192 -t 6 --reasoning off
```

`denwa-sts` の既定は `http://127.0.0.1:8080/v1/chat/completions`。

## いちばん簡単（推奨）

リポジトリ直下の PowerShell で:

```powershell
.\scripts\deploy-proxmox.ps1
```

## 手動（コマンドだけ控えたいとき）

### 1. Windows（リポジトリ直下）

```powershell
cd C:\Users\shumi\Documents\omocha_nobackup\denwa-sts

ssh -o BatchMode=yes proxmox "rm -rf /tmp/denwa-sts-build && mkdir -p /tmp/denwa-sts-build"

tar -cf - --exclude=.git --exclude=denwa-sts --exclude=sts --exclude=*.exe . `
  | ssh proxmox "cd /tmp/denwa-sts-build && tar -xf -"

ssh proxmox "chmod +x /tmp/denwa-sts-build/scripts/proxmox-build.sh && DENWA_STS_BUILD_DIR=/tmp/denwa-sts-build bash /tmp/denwa-sts-build/scripts/proxmox-build.sh"
```

### 2. Proxmox 上だけ（ソースが既に `/tmp/denwa-sts-build` にある場合）

```bash
DENWA_STS_BUILD_DIR=/tmp/denwa-sts-build bash /tmp/denwa-sts-build/scripts/proxmox-build.sh
```

または同等の一行:

```bash
export PATH="$HOME/.local/go/bin:$PATH"
export CGO_ENABLED=1
export CGO_CFLAGS="-I$HOME/voicevox/core/include"
export CGO_LDFLAGS="-L$HOME/voicevox/core/lib -Wl,-rpath,$HOME/voicevox/core/lib"
cd /tmp/denwa-sts-build
bash scripts/setup-piper.sh
go test ./...
go build -o denwa-sts ./cmd/denwa-sts
install -m 755 denwa-sts /home/denwa/sts/denwa-sts
pkill -x denwa-sts || true
sleep 1
cd /home/denwa/sts
nohup ./denwa-sts >>denwa_sts.out 2>&1 &
```

## 動作確認

```powershell
ssh proxmox "pgrep -a denwa-sts; tail -n 20 /home/denwa/sts/denwa_sts.log"
```

`起動 app=denwa_sts` と `読み上げ暖機完了` が出ていればよい。