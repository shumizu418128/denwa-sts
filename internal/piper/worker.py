"""Piper を常駐させ、長さ付きの PCM16 を返す。

Piper の CLI は起動のたびにモデルを読み、raw 出力には文の区切りが無い。
このプロセスはモデルを1回だけ読み、次の形式でやり取りする。数値はリトルエンディアン。

起動: stdout に READY\\n
要求: uint32 バイト数 + UTF-8。バイト数 0 は終了。
成功: uint32 0, uint32 サンプルレート, uint32 バイト数, PCM16 モノラル
失敗: uint32 1, uint32 バイト数, UTF-8 のメッセージ
"""

from __future__ import annotations

import argparse
import struct
import sys

import onnxruntime

from piper import PiperVoice

MAX_TEXT_BYTES = 8000


def read_exact(n: int) -> bytes | None:
    buf = b""
    while len(buf) < n:
        chunk = sys.stdin.buffer.read(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf


def write_bytes(data: bytes) -> None:
    sys.stdout.buffer.write(data)
    sys.stdout.buffer.flush()


def write_error(message: str) -> None:
    payload = message.encode("utf-8", errors="replace")
    write_bytes(struct.pack("<II", 1, len(payload)) + payload)


def load_voice(model_path: str, threads: int) -> PiperVoice:
    voice = PiperVoice.load(model_path)
    if threads > 0:
        options = onnxruntime.SessionOptions()
        options.intra_op_num_threads = threads
        options.inter_op_num_threads = 1
        # load はスレッド数を指定できないので、同じモデルでセッションを作り直す。
        old = voice.session
        voice.session = onnxruntime.InferenceSession(
            model_path,
            sess_options=options,
            providers=["CPUExecutionProvider"],
        )
        del old
    return voice


def synthesize(voice: PiperVoice, text: str) -> tuple[int, bytes]:
    rate = voice.config.sample_rate
    parts: list[bytes] = []
    for chunk in voice.synthesize(text):
        if chunk.sample_channels != 1 or chunk.sample_width != 2:
            raise RuntimeError("モノラルの PCM16 以外は扱えません")
        rate = chunk.sample_rate
        parts.append(chunk.audio_int16_bytes)
    return rate, b"".join(parts)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--threads", type=int, default=2)
    args = parser.parse_args()

    onnxruntime.set_default_logger_severity(3)
    voice = load_voice(args.model, args.threads)
    print(f"音声モデルを読み込みました: {args.model}", file=sys.stderr, flush=True)
    write_bytes(b"READY\n")

    while True:
        header = read_exact(4)
        if header is None:
            return
        (size,) = struct.unpack("<I", header)
        if size == 0:
            return
        if size > MAX_TEXT_BYTES:
            write_error("文章が長すぎます")
            return
        raw = read_exact(size)
        if raw is None:
            return
        try:
            rate, pcm = synthesize(voice, raw.decode("utf-8"))
        except Exception as exc:
            write_error(f"{type(exc).__name__}: {exc}")
            continue
        write_bytes(struct.pack("<III", 0, rate, len(pcm)) + pcm)


if __name__ == "__main__":
    try:
        main()
    except BrokenPipeError:
        sys.exit(0)
