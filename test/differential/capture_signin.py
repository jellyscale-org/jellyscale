#!/usr/bin/env python3
"""Records how a real Jellyfin 12.4 and its jellyfin-web treat failed sign-ins (JS-145, D-020).

Usage: python3 -I test/differential/capture_signin.py <out.json>   (run from the repository root)
Environment: PLAYWRIGHT_DIR, a directory with playwright-core installed (default ~/.cache/jellyscale/pw);
CHROMIUM_PATH (default /usr/bin/chromium).

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with no library, completes the startup wizard and
asks it, with synthetic accounts:
  default   14 wrong passwords in a row for an account with the default LoginAttemptsBeforeLockout, then the right one;
  lockout   an account with LoginAttemptsBeforeLockout set to 3: wrong passwords until it answers differently, then a right
            one, a wrong one, its policy, and the right one again after an administrator enables it;
  unknown   a name that does not exist, against a wrong password, 10 times each (milliseconds, logged and not asserted);
and then has the container's own jellyfin-web, in Chromium, sign in while the answer of /Users/AuthenticateByName is
replaced by 401, 403, 429 and 503, each with and without Retry-After, recording what the page shows.
The container is removed when the script ends.

compat/captures/signin-12.4.json is read by internal/api/signin_capture_test.go.
"""
import http.client
import json
import os
import signal
import subprocess
import sys
import time

IMAGE = "jellyfin/jellyfin:12.4"
PASSWORD = "capture-signin-pass"
DEVICE = 'MediaBrowser Client="capture", Device="capture", DeviceId="capture-signin", Version="1"'
JSON = {"Content-Type": "application/json"}


def run(*cmd, **kw):
    return subprocess.run(cmd, check=True, capture_output=True, text=True, **kw).stdout.strip()


def request(port, method, path, headers=None, body=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=120)
    began = time.perf_counter()
    conn.request(method, path, body=body, headers=headers or {})
    resp = conn.getresponse()
    data = resp.read()
    ms = (time.perf_counter() - began) * 1000
    out = (resp.status, {k.lower(): v for k, v in resp.getheaders()}, data, ms)
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
    here = os.path.dirname(os.path.abspath(__file__))
    name = f"jellyscale-capture-signin-{os.getpid()}"
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        run("docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])

        def up():
            try:
                status, _, data, _ = request(port, "GET", "/System/Info/Public")
                return json.loads(data) if status == 200 and b"Version" in data else None
            except (OSError, http.client.HTTPException):
                return None
        version = wait_for("jellyfin to start", up)["Version"]
        for method, path, body in (("POST", "/Startup/Configuration", '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", json.dumps({"Name": "admin", "Password": PASSWORD})),
                                   ("POST", "/Startup/RemoteAccess", '{"EnableRemoteAccess":true}'), ("POST", "/Startup/Complete", None)):
            status, _, data, _ = request(port, method, path, JSON, body)
            if status >= 300:
                raise SystemExit(f"{method} {path}: {status} {data[:200]!r}")

        def signin(user, password, device="capture-signin"):
            auth = DEVICE.replace("capture-signin", device)
            body = json.dumps({"Username": user, "Pw": password})
            return request(port, "POST", "/Users/AuthenticateByName", {**JSON, "Authorization": auth}, body)

        def row(reply):
            status, headers, data, ms = reply
            kept = {k: v for k, v in headers.items() if k in ("retry-after", "content-type", "www-authenticate")}
            return dict(status=status, ms=round(ms, 1), headers=kept, body=data.decode("utf-8", "replace")[:200])

        status, _, data, _ = signin("admin", PASSWORD)
        admin = json.loads(data)
        auth = {"Authorization": DEVICE.replace('Version="1"', f'Version="1", Token="{admin["AccessToken"]}"')}

        def make_user(user):
            status, _, data, _ = request(port, "POST", "/Users/New", {**JSON, **auth}, json.dumps({"Name": user, "Password": PASSWORD}))
            if status >= 300:
                raise SystemExit(f"creating {user}: {status} {data[:200]!r}")
            return json.loads(data)["Id"]

        def policy(user_id, **changes):
            status, _, data, _ = request(port, "GET", f"/Users/{user_id}", auth)
            current = json.loads(data)["Policy"]
            current.update(changes)
            status, _, data, _ = request(port, "POST", f"/Users/{user_id}/Policy", {**JSON, **auth}, json.dumps(current))
            if status >= 300:
                raise SystemExit(f"policy of {user_id}: {status} {data[:200]!r}")

        def policy_of(user_id):
            status, _, data, _ = request(port, "GET", f"/Users/{user_id}", auth)
            p = json.loads(data)["Policy"]
            return dict(IsDisabled=p["IsDisabled"], LoginAttemptsBeforeLockout=p["LoginAttemptsBeforeLockout"])

        result = dict(image=IMAGE, server_version=version)

        make_user("capture-default")
        default = dict(policy=policy_of(next(u["Id"] for u in json.loads(request(port, "GET", "/Users", auth)[2]) if u["Name"] == "capture-default")))
        default["wrong"] = [row(signin("capture-default", f"wrong-{i}", f"device-{i}")) for i in range(14)]
        default["right"] = row(signin("capture-default", PASSWORD, "device-right"))
        result["default"] = default

        limit = 3
        locked = make_user("capture-lockout")
        policy(locked, LoginAttemptsBeforeLockout=limit)
        lockout = dict(limit=limit, policy_before=policy_of(locked), wrong=[])
        for i in range(limit + 3):
            lockout["wrong"].append(row(signin("capture-lockout", f"wrong-{i}")))
        lockout["right_after"] = row(signin("capture-lockout", PASSWORD))
        lockout["wrong_after"] = row(signin("capture-lockout", "wrong-again"))
        lockout["policy_after"] = policy_of(locked)
        time.sleep(30)
        lockout["right_after_30s"] = row(signin("capture-lockout", PASSWORD))
        policy(locked, IsDisabled=False)
        lockout["policy_enabled"] = policy_of(locked)
        lockout["right_enabled"] = row(signin("capture-lockout", PASSWORD))
        result["lockout"] = lockout

        unknown = dict(unknown=[], wrong_password=[])
        for i in range(10):
            unknown["unknown"].append(row(signin(f"nobody-{i}", "wrong")))
            unknown["wrong_password"].append(row(signin("capture-default" if False else "admin", f"wrong-{i}", f"device-u{i}")))
        result["unknown_name"] = unknown

        env = dict(os.environ, BASE=f"http://127.0.0.1:{port}")
        env.setdefault("CHROMIUM_PATH", "/usr/bin/chromium")
        pw = os.environ.get("PLAYWRIGHT_DIR", os.path.expanduser("~/.cache/jellyscale/pw"))
        web = subprocess.run(["node", os.path.join(here, "capture_signin_web.mjs")], cwd=pw, env=env, capture_output=True, text=True)
        sys.stderr.write(web.stderr)
        if web.returncode != 0:
            raise SystemExit(f"capture_signin_web.mjs failed:\n{web.stdout}")
        result["web"] = json.loads(web.stdout)

        with open(out, "w") as f:
            json.dump(result, f, indent=1)
            f.write("\n")
        print(json.dumps(result, indent=1)[:6000])
    finally:
        if started:
            remove_container(name)


if __name__ == "__main__":
    main()
