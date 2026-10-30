#!/usr/bin/env python3
"""Tests whether players follow a 302 from /stream to an S3-like origin.

The origin behaves like a presigned URL: it answers ranged GETs, returns 403 to
HEAD as both real stores did, and logs any Authorization header it receives.

Usage: run.py <media_file>
"""
import http.server
import re
import subprocess
import sys
import threading
import time
from collections import Counter

media = sys.argv[1]
size = len(open(media, "rb").read(1)) and __import__("os").path.getsize(media)
hits = Counter()
auth_seen = []


class Origin(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_HEAD(self):
        hits["origin HEAD"] += 1
        self.send_response(403)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_OPTIONS(self):
        hits["origin OPTIONS"] += 1
        self.send_response(403)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        hits["origin GET"] += 1
        if self.headers.get("Authorization"):
            auth_seen.append(self.headers["Authorization"][:30])
        start, end = 0, size - 1
        match = re.match(r"bytes=(\d+)-(\d*)", self.headers.get("Range", ""))
        if match:
            start = int(match[1])
            end = min(int(match[2]) if match[2] else end, end)
        self.send_response(206 if match else 200)
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Type", "video/mp4")
        self.send_header("Content-Length", str(end - start + 1))
        if match:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()
        try:
            with open(media, "rb") as f:
                f.seek(start)
                remaining = end - start + 1
                while remaining:
                    chunk = f.read(min(65536, remaining))
                    self.wfile.write(chunk)
                    remaining -= len(chunk)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True

    def log_message(self, *args):
        pass


class Api(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def redirect(self):
        hits[f"api {self.command}"] += 1
        self.send_response(302)
        self.send_header("Location", f"http://127.0.0.1:{origin.server_port}/media.mp4?X-Amz-Signature=fake")
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_GET = do_HEAD = redirect

    def log_message(self, *args):
        pass


origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Origin)
api = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Api)
for server in (origin, api):
    threading.Thread(target=server.serve_forever, daemon=True).start()
url = f"http://127.0.0.1:{api.server_port}/Videos/x/stream.mp4?Static=true"


def trial(name, command):
    hits.clear()
    auth_seen.clear()
    start = time.monotonic()
    done = subprocess.run(command, capture_output=True, text=True, timeout=60)
    print(f"{name:<22} exit {done.returncode}  {time.monotonic() - start:5.1f}s  {dict(hits)}  auth_forwarded={len(auth_seen)}")
    if done.returncode:
        print("   ", (done.stderr or done.stdout).strip()[-160:])


header = 'Authorization: MediaBrowser Token="abc"'
trial("ffmpeg seek", ["ffmpeg", "-nostdin", "-v", "error", "-ss", "60", "-i", url, "-t", "2", "-f", "null", "-"])
trial("ffmpeg + auth header", ["ffmpeg", "-nostdin", "-v", "error", "-headers", header + "\r\n", "-ss", "60", "-i", url,
                               "-t", "2", "-f", "null", "-"])
trial("mpv seek", ["mpv", "--no-config", "--vo=null", "--ao=null", "--start=60", "--length=2", "--really-quiet", url])
trial("mpv + auth header", ["mpv", "--no-config", "--vo=null", "--ao=null", "--start=60", "--length=2", "--really-quiet",
                            f"--http-header-fields={header}", url])
trial("gstreamer", ["timeout", "20", "gst-launch-1.0", "-q", "souphttpsrc", f"location={url}", "!", "qtdemux", "name=d",
                    "d.video_0", "!", "fakesink", "sync=false"])
page = f"""<!doctype html><video id=v muted src="{url}"></video><script>
const v=document.getElementById('v');window.r={{}};
v.addEventListener('error',()=>{{window.r.error=v.error?.message||'error';window.r.done=1}});
v.addEventListener('canplay',()=>{{if(!window.r.s){{window.r.s=1;v.currentTime=60;v.play()}}}});
setInterval(()=>{{if(v.currentTime>62){{window.r.t=v.currentTime;window.r.done=1}}}},200);</script>"""
open(sys.argv[2], "w").write(page) if len(sys.argv) > 2 else None
print(f"API {api.server_port}")
if len(sys.argv) > 3:
    hits.clear()
    auth_seen.clear()
    done = subprocess.run(["node", sys.argv[3], sys.argv[2]], capture_output=True, text=True, timeout=60)
    print(f"chromium <video>        {done.stdout.strip()}  {dict(hits)}")
