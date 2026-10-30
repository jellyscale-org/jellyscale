#!/usr/bin/env python3
"""Measure the HTTP range traffic ffprobe and ffmpeg generate per file layout.

Stands in for an S3 endpoint: each request approximates one ranged GET. Bytes
are what was actually written before the client hung up, because ffmpeg opens
long ranges and disconnects early.

Usage: measure.py <workdir>
"""
import http.server
import re
import subprocess
import sys
import threading
import time
from pathlib import Path

work = Path(sys.argv[1])
work.mkdir(parents=True, exist_ok=True)
LATENCY_S = 0.02  # Rough time to first byte for an object store.
log = []


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def serve(self, body):
        time.sleep(LATENCY_S)
        path = work / self.path.lstrip("/")
        size = path.stat().st_size
        start, end = 0, size - 1
        match = re.match(r"bytes=(\d+)-(\d*)", self.headers.get("Range", ""))
        if match:
            start = int(match[1])
            end = min(int(match[2]) if match[2] else end, end)
        self.send_response(206 if match else 200)
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(end - start + 1))
        if match:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()
        entry = [self.command, start, 0]
        log.append(entry)
        if not body:
            return
        with path.open("rb") as f:
            f.seek(start)
            remaining = end - start + 1
            try:
                while remaining:
                    chunk = f.read(min(65536, remaining))
                    self.wfile.write(chunk)
                    entry[2] += len(chunk)
                    remaining -= len(chunk)
            except (BrokenPipeError, ConnectionResetError):
                self.close_connection = True

    def log_message(self, *args):
        pass


def make_sources():
    src = work / "base.mkv"
    if not src.exists():
        subprocess.run(
            ["ffmpeg", "-nostdin", "-v", "error", "-y",
             "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24000/1001:duration=600",
             "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=600",
             "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-g", "240",
             "-c:a", "aac", str(src)], check=True)
    variants = {"front.mp4": ["-movflags", "+faststart"], "end.mp4": [], "cues.mkv": []}
    for name, extra in variants.items():
        if not (work / name).exists():
            subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-i", str(src), "-c", "copy",
                            *extra, str(work / name)], check=True)
    if not (work / "nocues.mkv").exists():
        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-i", str(src), "-c", "copy",
                        "-cues_to_front", "0", "-reserve_index_space", "0",
                        "-write_crc32", "0", "-f", "matroska", "-live", "1",
                        str(work / "nocues.mkv")], check=True)


def measure(label, command):
    log.clear()
    start = time.monotonic()
    done = subprocess.run(command, capture_output=True, text=True)
    elapsed = time.monotonic() - start
    time.sleep(0.2)
    total = sum(e[2] for e in log)
    state = "ok" if done.returncode == 0 else f"exit {done.returncode}"
    print(f"{label:<26} {len(log):>5} {total / 1e6:>10.2f} {elapsed:>7.2f}s  {state}")


def main():
    make_sources()
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_port}"
    size = (work / "base.mkv").stat().st_size
    print(f"file about {size / 1e6:.0f}MB, 600s, {LATENCY_S * 1000:.0f}ms injected latency per request")
    print(f"{'operation':<26} {'reqs':>5} {'MB read':>10} {'time':>8}")
    for name in ("front.mp4", "end.mp4", "cues.mkv", "nocues.mkv"):
        url = f"{base}/{name}"
        measure(f"{name} probe", ["ffprobe", "-v", "error", "-show_format", "-show_streams",
                                  "-of", "json", url])
        measure(f"{name} seek to 300s", ["ffmpeg", "-nostdin", "-v", "error", "-ss", "300", "-i", url,
                                         "-t", "4", "-c", "copy", "-f", "null", "-"])


if __name__ == "__main__":
    main()
