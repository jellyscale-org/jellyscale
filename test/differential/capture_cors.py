#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 answers to a cross-origin request (JS-144, D-016).

Usage: python3 -I test/differential/capture_cors.py <out.json>   (run from the repository root)

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port over one synthetic film made here with ffmpeg,
completes the startup wizard and signs an administrator in. Then it asks each request of REQUESTS three times: with
the Origin ORIGINS[0], with ORIGINS[1] and with no Origin at all, and records the status and every Access-Control-*,
Vary and Allow header of each answer, and the length of its body. The container is removed when the script ends.

internal/api/cors_integration_test.go holds the result to what jellyscale answers to the same requests, and every difference to
compat/differences.yaml.
"""
import http.client
import json
import os
import shutil
import signal
import subprocess
import sys
import time

IMAGE = "jellyfin/jellyfin:12.4"
PASSWORD = "capture-cors-pass"
ORIGINS = ["tauri://localhost", "https://app.example.test"]
DEVICE = 'MediaBrowser Client="capture", Device="capture", DeviceId="capture-cors", Version="1"'
# The two request headers a page's script sets on a simple GET it sends with a token.
SIX = "authorization,content-type,range,x-emby-authorization,x-emby-token,x-mediabrowser-token"

# name, method, path ({film} is the film's id), headers other than Origin, token carried in an Authorization header
REQUESTS = [
    ("open route", "GET", "/System/Info/Public", {}, False),
    ("viewer route with a token", "GET", "/UserViews", {}, True),
    ("viewer route with no token", "GET", "/UserViews", {}, False),
    ("ranged film", "GET", "/Videos/{film}/stream?static=true&mediaSourceId={film}", {"Range": "bytes=100-199"}, True),
    ("preflight of the sign-in", "OPTIONS", "/Users/AuthenticateByName",
     {"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type"}, False),
    ("preflight asking range", "OPTIONS", "/Videos/{film}/stream?static=true&mediaSourceId={film}",
     {"Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "range"}, False),
    ("preflight asking all six headers", "OPTIONS", "/Items/{film}",
     {"Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": SIX}, False),
    ("preflight asking DELETE", "OPTIONS", "/Videos/ActiveEncodings",
     {"Access-Control-Request-Method": "DELETE", "Access-Control-Request-Headers": "authorization"}, False),
    ("preflight asking PATCH", "OPTIONS", "/Items/{film}",
     {"Access-Control-Request-Method": "PATCH", "Access-Control-Request-Headers": "authorization,content-type"}, False),
    ("preflight asking a header outside the six", "OPTIONS", "/Users/AuthenticateByName",
     {"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,x-capture-extra"}, False),
    ("GET of an unserved path", "GET", "/Capture/Unserved", {}, False),
    ("preflight of an unserved path", "OPTIONS", "/Capture/Unserved",
     {"Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"}, False),
]


def run(*cmd):
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout.strip()


def request(port, method, path, headers=None, body=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=120)
    conn.request(method, path, body=body, headers=headers or {})
    resp = conn.getresponse()
    data = resp.read() if method != "HEAD" else b""
    out = (resp.status, [(k.lower(), v) for k, v in resp.getheaders()], data)
    conn.close()
    return out


def wait_for(what, check, seconds=180):
    for _ in range(seconds):
        value = check()
        if value:
            return value
        time.sleep(1)
    raise SystemExit(f"timed out waiting for {what}")


def remove_container(name):
    for _ in range(10):
        subprocess.run(["docker", "rm", "-fv", name], capture_output=True)
        if subprocess.run(["docker", "inspect", name], capture_output=True).returncode != 0:
            return
        time.sleep(1)
    raise SystemExit(f"could not remove the container {name}")


def main():
    out = os.path.abspath(sys.argv[1])
    work = os.path.join(os.path.expanduser("~/.cache/jellyscale/agents"), f"capture-cors-{os.getpid()}")
    film_dir = os.path.join(work, "movies", "Alpha Film (2021)")
    os.makedirs(film_dir)
    name = f"jellyscale-capture-cors-{os.getpid()}"
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=12",
                        "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=12", "-c:v", "libx264", "-preset", "ultrafast",
                        "-g", "24", "-c:a", "aac", os.path.join(film_dir, "Alpha Film (2021).mkv")], check=True)
        run("docker", "run", "-d", "--rm", "--name", name, "-v", f"{work}:/media:ro", "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])

        def up():
            try:
                status, _, data = request(port, "GET", "/System/Info/Public")
                return json.loads(data) if status == 200 and b"Version" in data else None
            except (OSError, http.client.HTTPException):
                return None
        version = wait_for("jellyfin to start", up)["Version"]
        json_headers = {"Content-Type": "application/json"}
        for method, path, body in (("POST", "/Startup/Configuration", '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", json.dumps({"Name": "admin", "Password": PASSWORD})),
                                   ("POST", "/Startup/RemoteAccess", '{"EnableRemoteAccess":true}'), ("POST", "/Startup/Complete", None)):
            status, _, data = request(port, method, path, json_headers, body)
            if status >= 300:
                raise SystemExit(f"{method} {path}: {status} {data[:200]!r}")
        status, _, data = request(port, "POST", "/Users/AuthenticateByName", {**json_headers, "Authorization": DEVICE}, json.dumps({"Username": "admin", "Pw": PASSWORD}))
        token = json.loads(data)["AccessToken"]
        auth = {"Authorization": DEVICE.replace('Version="1"', f'Version="1", Token="{token}"')}
        request(port, "POST", "/Library/VirtualFolders?refreshLibrary=true&name=Movies&collectionType=movies", {**json_headers, **auth},
                json.dumps({"LibraryOptions": {"PathInfos": [{"Path": "/media/movies"}]}}))

        def film():
            status, _, data = request(port, "GET", "/Items?Recursive=true&IncludeItemTypes=Movie", auth)
            items = json.loads(data)["Items"] if status == 200 else []
            return items[0]["Id"] if items else None
        film_id = wait_for("the scan", film, 240)

        rows = []
        for label, method, path, headers, with_token in REQUESTS:
            for origin in ORIGINS + [None]:
                sent = dict(headers)
                if origin:
                    sent["Origin"] = origin
                if with_token:
                    sent.update(auth)
                status, answer, data = request(port, method, path.format(film=film_id), sent)
                kept = {}
                for key, value in answer:
                    if key.startswith("access-control-") or key in ("vary", "allow"):
                        kept[key] = value
                rows.append(dict(request=label, method=method, path=path, origin=origin, sent=headers, token=with_token,
                                 status=status, headers=kept, body_bytes=len(data)))
                print(label, origin, status, kept, flush=True)
        with open(out, "w") as f:
            json.dump(dict(image=IMAGE, server_version=version, origins=ORIGINS, rows=rows), f, indent=1)
            f.write("\n")
    finally:
        if started:
            remove_container(name)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
