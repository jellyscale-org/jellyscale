#!/usr/bin/env python3
"""Records how a real Jellyfin 12.4 and its jellyfin-web treat Quick Connect (JS-037).

Usage: python3 -I test/differential/capture_quick_connect.py <out.json>   (run from the repository root)
Environment: PLAYWRIGHT_DIR, a directory with playwright-core installed (default ~/.cache/jellyscale/pw);
CHROMIUM_PATH (default /usr/bin/chromium). It takes about 22 minutes: the expiry rows wait for real time.

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with no library, completes the startup wizard, makes
synthetic accounts (admin, viewer, other, gone), and
  steps    asks the five Quick Connect routes, in order, in each state: on (the default), a code pending, approved and
           redeemed, unknown codes and secrets, Initiate without each device field and with stale tokens, every way of
           naming an account in an approval, a device already signed in, an account deleted or disabled after approving,
           and Quick Connect turned off and on again (the server's QuickConnectAvailable setting);
  web      has the container's own jellyfin-web, in Chromium, sign a second browser in with a code (on), show the Quick
           Connect page to a viewer and an administrator, answer a refused Initiate with each status, and show the pages
           with Quick Connect off;
  expiry   times how long a pending code and an approved one live, from Initiate and from approval.
The container is removed when the script ends.

compat/captures/quick-connect-12.4.json is read by cmd/jellyscale/quickconnect_capture_integration_test.go, which asks
jellyscale the "steps" requests in order. Placeholders in it, {code:x}, {secret:x}, {token:x}, {userid:name}, stand for
the values the server made.
"""
import http.client
import json
import os
import re
import signal
import subprocess
import sys
import time

IMAGE = "jellyfin/jellyfin:12.4"
PASSWORD = "capture-quick-connect-pass"
JSON_HEADERS = {"Content-Type": "application/json"}
KEPT_HEADERS = ("retry-after", "content-type", "www-authenticate")


def run(*cmd, **kw):
    return subprocess.run(cmd, check=True, capture_output=True, text=True, **kw).stdout.strip()


def request(port, method, path, headers=None, body=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=120)
    conn.request(method, path, body=body, headers=headers or {})
    resp = conn.getresponse()
    data = resp.read()
    out = (resp.status, {k.lower(): v for k, v in resp.getheaders()}, data)
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


def device_header(device, token=None):
    value = f'MediaBrowser Client="capture", Device="TV-{device}", DeviceId="dev-{device}", Version="1.0"'
    if token:
        value += f', Token="{token}"'
    return value


class Recorder:
    """Asks the server one step at a time and records each with the server's values replaced by placeholders."""

    def __init__(self, port):
        self.port = port
        self.values = {}  # placeholder -> the server's value
        self.steps = []
        self.section = ""

    def fill(self, text):
        return re.sub(r"\{(code|secret|token|userid|userid-dashed):([\w-]+)\}", self.value_of, text)

    def value_of(self, match):
        kind, name = match.group(1), match.group(2)
        if kind == "userid-dashed":
            raw = self.values["{userid:%s}" % name]
            return f"{raw[:8]}-{raw[8:12]}-{raw[12:16]}-{raw[16:20]}-{raw[20:]}".upper()
        return self.values[match.group(0)]

    def hide(self, text):
        for placeholder, value in sorted(self.values.items(), key=lambda item: -len(item[1])):
            text = text.replace(value, placeholder)
        return text

    def recorded_body(self, data, content_type):
        text = data.decode("utf-8", "replace")
        if "json" not in content_type or not text:
            return self.hide(text)[:300]
        body = json.loads(text)
        if isinstance(body, dict):
            body.pop("traceId", None)
        return json.loads(self.hide(json.dumps(body)))

    def ask(self, name, method, path, device=None, token=None, headers=None, body=None, saves=None, pick=None, expect=None):
        """One request. device and token make the Authorization header; token is a placeholder name."""
        sent = dict(headers or {})
        if device:
            sent["Authorization"] = device_header(device, "{token:%s}" % token if token else None)
        if body is not None:
            sent["Content-Type"] = "application/json"
        filled = {key: self.fill(value) for key, value in sent.items()}
        status, reply_headers, data = request(self.port, method, self.fill(path), filled, None if body is None else self.fill(body))
        content_type = reply_headers.get("content-type", "")
        if saves and status == 200:
            reply = json.loads(data)
            if "Secret" in reply:
                self.values["{secret:%s}" % saves] = reply["Secret"]
                self.values["{code:%s}" % saves] = reply["Code"]
            if "AccessToken" in reply and reply["AccessToken"] not in self.values.values():
                self.values["{token:%s}" % saves] = reply["AccessToken"]
            elif "AccessToken" in reply:
                same = next(key for key, value in self.values.items() if value == reply["AccessToken"])
                self.values["{token:%s}" % saves] = self.values[same]
        if pick is not None and status == 200:
            rows = [row for row in json.loads(data) if row.get("DeviceId") == pick]
            data = json.dumps([{k: row.get(k) for k in ("DeviceId", "DeviceName", "Client", "ApplicationVersion", "UserName")} for row in rows]).encode()
        step = dict(section=self.section, name=name, action="request", method=method, path=path, headers=sent, body=body,
                    saves=saves, pick=pick, status=status,
                    response_headers={k: v for k, v in reply_headers.items() if k in KEPT_HEADERS},
                    response=self.recorded_body(data, content_type))
        if any(other["name"] == name for other in self.steps):
            raise SystemExit(f"two steps are named {name!r}")
        self.steps.append(step)
        print(f"{self.section}: {name}: {status} {json.dumps(step['response'])[:160]}", file=sys.stderr)
        if expect is not None and status != expect:
            raise SystemExit(f"{name}: {status}, the capture's setup needs {expect}")
        return status, data

    def action(self, kind, **fields):
        self.steps.append(dict(section=self.section, name=f"{kind} {len(self.steps)}", action=kind, **fields))

    # Shorthands for the steps the sections repeat.
    def initiate(self, name, code, device=None, **kw):
        return self.ask(name, "POST", "/QuickConnect/Initiate", device=device or code, saves=code, expect=200, **kw)

    def connect(self, name, code, **kw):
        return self.ask(name, "GET", "/QuickConnect/Connect?Secret={secret:%s}" % code, **kw)

    def authorize(self, name, code, approver, query="", **kw):
        return self.ask(name, "POST", "/QuickConnect/Authorize?code={code:%s}%s" % (code, query), device="approver-" + approver, token=approver, **kw)

    def redeem(self, name, code, device=None, token_name=None, **kw):
        return self.ask(name, "POST", "/Users/AuthenticateWithQuickConnect", device=device if device is not None else code,
                        body='{"Secret":"{secret:%s}"}' % code, saves=token_name, **kw)

    def me(self, name, token):
        return self.ask(name, "GET", "/Users/Me", device="probe", token=token)

    def password_signin(self, user, device, token):
        status, _, data = request(self.port, "POST", "/Users/AuthenticateByName",
                                  {**JSON_HEADERS, "Authorization": device_header(device)}, json.dumps({"Username": user, "Pw": PASSWORD}))
        if status != 200:
            raise SystemExit(f"signing in {user}: {status} {data[:200]!r}")
        reply = json.loads(data)
        self.values["{token:%s}" % token] = reply["AccessToken"]
        self.values.setdefault("{userid:%s}" % user, reply["User"]["Id"])
        self.action("password_signin", user=user, device=device, token=token)


def set_enabled(port, admin_token, enabled):
    auth = {"Authorization": device_header("capture-admin", admin_token)}
    status, _, data = request(port, "GET", "/System/Configuration", auth)
    config = json.loads(data)
    before = config["QuickConnectAvailable"]
    config["QuickConnectAvailable"] = enabled
    status, _, data = request(port, "POST", "/System/Configuration", {**JSON_HEADERS, **auth}, json.dumps(config))
    if status >= 300:
        raise SystemExit(f"setting QuickConnectAvailable: {status} {data[:200]!r}")
    return before


def state_steps(rec, port):
    rec.section = "on"
    rec.password_signin("admin", "approver-admin", "admin")
    rec.password_signin("viewer", "approver-viewer", "viewer")
    rec.password_signin("other", "approver-other", "other")
    rec.ask("enabled by default", "GET", "/QuickConnect/Enabled")
    rec.initiate("initiate", "tv1")
    rec.connect("connect pending", "tv1")
    rec.ask("connect unknown secret", "GET", "/QuickConnect/Connect?Secret=0000deadbeef", device="tv1")
    rec.ask("connect without a secret", "GET", "/QuickConnect/Connect", device="tv1")
    rec.redeem("redeem pending", "tv1")
    rec.ask("redeem unknown secret", "POST", "/Users/AuthenticateWithQuickConnect", device="tv1", body='{"Secret":"0000deadbeef"}')
    rec.ask("redeem an empty secret", "POST", "/Users/AuthenticateWithQuickConnect", device="tv1", body='{"Secret":""}')
    rec.ask("redeem without a secret", "POST", "/Users/AuthenticateWithQuickConnect", device="tv1", body="{}")
    rec.ask("approve an unknown code", "POST", "/QuickConnect/Authorize?code=000001", device="approver-viewer", token="viewer")
    rec.ask("approve without a code", "POST", "/QuickConnect/Authorize", device="approver-viewer", token="viewer")
    rec.ask("approve without a token", "POST", "/QuickConnect/Authorize?code={code:tv1}", device="approver-anon")
    rec.authorize("viewer names another user", "tv1", "viewer", "&userId={userid:other}")
    rec.connect("connect after a refused approval", "tv1")
    rec.authorize("viewer approves naming themselves", "tv1", "viewer", "&userId={userid:viewer}", expect=200)
    rec.connect("connect approved", "tv1")
    rec.authorize("approve an approved code again", "tv1", "viewer", "&userId={userid:viewer}")
    rec.authorize("another account approves an approved code", "tv1", "other")
    rec.redeem("redeem approved", "tv1", token_name="tv1-first", expect=200)
    rec.redeem("redeem a second time", "tv1", token_name="tv1-second")
    rec.me("first token after the second redemption", "tv1-first")
    rec.me("second token", "tv1-second")
    rec.connect("connect after redemption", "tv1")
    rec.authorize("approve after redemption", "tv1", "viewer")

    rec.section = "initiate"
    fields = {"Client": "capture", "Device": "TV-field", "DeviceId": "dev-field", "Version": "1.0"}
    for missing in fields:
        value = "MediaBrowser " + ", ".join(f'{key}="{val}"' for key, val in fields.items() if key != missing)
        rec.ask(f"initiate without {missing}", "POST", "/QuickConnect/Initiate", headers={"Authorization": value})
        value = "MediaBrowser " + ", ".join(f'{key}="{"" if key == missing else val}"' for key, val in fields.items())
        rec.ask(f"initiate with an empty {missing}", "POST", "/QuickConnect/Initiate", headers={"Authorization": value})
    rec.ask("initiate with no Authorization", "POST", "/QuickConnect/Initiate")
    rec.password_signin("viewer", "to-revoke", "revoked")
    rec.ask("sign the token out", "POST", "/Sessions/Logout", device="to-revoke", token="revoked")
    rec.values["{token:unknown}"] = "0123456789abcdef0123456789abcdef"
    for kind in ("unknown", "revoked", "other"):
        code = f"stale-{kind}"
        rec.initiate(f"initiate with the {kind} token", code, token=kind)
        rec.connect(f"connect with the {kind} token", code, device=code, token=kind)
        rec.authorize(f"approve the code initiated with the {kind} token", code, "viewer", expect=200)
        rec.redeem(f"redeem with the {kind} token", code, device=code, token_name=code, headers={"X-Emby-Token": "{token:%s}" % kind})
    rec.me("the other account's token after being sent along", "other")

    rec.section = "naming"
    rec.initiate("initiate for the administrator", "adm-self")
    rec.authorize("administrator approves naming themselves", "adm-self", "admin", "&userId={userid:admin}")
    rec.redeem("redeem the administrator's own approval", "adm-self", token_name="adm-self")
    rec.initiate("initiate for naming another", "adm-other")
    rec.authorize("administrator names another user", "adm-other", "admin", "&userId={userid:other}")
    rec.connect("connect after the administrator named another", "adm-other")
    rec.redeem("redeem the approval naming another", "adm-other", token_name="adm-other")
    rec.initiate("initiate for no userId", "adm-none")
    rec.authorize("administrator approves with no userId", "adm-none", "admin")
    rec.redeem("redeem with no userId named", "adm-none", token_name="adm-none")
    rec.initiate("initiate for an empty userId", "adm-empty")
    rec.authorize("administrator approves with an empty userId", "adm-empty", "admin", "&userId=")
    rec.redeem("redeem the administrator's empty userId", "adm-empty", token_name="adm-empty")
    rec.initiate("initiate for the viewer's empty userId", "view-empty")
    rec.authorize("viewer approves with an empty userId", "view-empty", "viewer", "&userId=")
    rec.redeem("redeem the viewer's empty userId", "view-empty", token_name="view-empty")
    rec.initiate("initiate for unknown users", "unknown-user")
    rec.authorize("administrator names a user nobody is", "unknown-user", "admin", "&userId=11111111111111111111111111111111")
    rec.initiate("initiate for the viewer naming unknown users", "unknown-user2")
    rec.authorize("viewer names a user nobody is", "unknown-user2", "viewer", "&userId=11111111111111111111111111111111")
    rec.authorize("viewer names a malformed userId", "unknown-user2", "viewer", "&userId=zzz")
    rec.initiate("initiate for a dashed userId", "view-dashed")
    rec.authorize("viewer names themselves dashed and in capitals", "view-dashed", "viewer", "&userId={userid-dashed:viewer}")
    rec.initiate("initiate for a spaced code", "spaced")
    code = rec.values["{code:spaced}"]
    rec.values["{code:spaced-out}"] = code[:3] + "%20" + code[3:]
    rec.authorize("approve a code with a space in it", "spaced-out", "viewer")

    rec.section = "devices"
    rec.password_signin("viewer", "same-account", "same-old")
    rec.initiate("initiate on a device signed in to the same account", "same-account")
    rec.authorize("approve the same account", "same-account", "viewer", expect=200)
    rec.redeem("redeem on a device signed in to the same account", "same-account", token_name="same-new", expect=200)
    rec.me("the device's old token of the same account", "same-old")
    rec.me("the device's new token", "same-new")
    rec.password_signin("other", "two-accounts", "two-other")
    rec.initiate("initiate on a device signed in to another account", "two-accounts")
    rec.authorize("approve a different account", "two-accounts", "viewer", expect=200)
    rec.redeem("redeem on a device signed in to another account", "two-accounts", token_name="two-new", expect=200)
    rec.me("the device's token of the other account", "two-other")
    rec.me("the device's new token beside it", "two-new")
    # What a password sign-in on one device does with the tokens it already holds, to compare.
    rec.password_signin("viewer", "by-password", "pw-viewer-old")
    rec.password_signin("other", "by-password", "pw-other")
    rec.password_signin("viewer", "by-password", "pw-viewer-new")
    rec.me("password: the first token of the account signed in again", "pw-viewer-old")
    rec.me("password: another account's token on the device", "pw-other")
    rec.me("password: the account's new token", "pw-viewer-new")
    rec.initiate("initiate for another header at redemption", "asker")
    rec.authorize("approve the asker", "asker", "viewer", expect=200)
    rec.redeem("redeem with another device's header", "asker", device="someone-else", token_name="asker", expect=200)
    rec.ask("the asker's session", "GET", "/Sessions", device="approver-admin", token="admin", pick="dev-asker")
    rec.ask("the other header's session", "GET", "/Sessions", device="approver-admin", token="admin", pick="dev-someone-else")
    rec.initiate("initiate for a redemption with no Authorization", "bare")
    rec.authorize("approve the bare redemption", "bare", "viewer", expect=200)
    rec.redeem("redeem with no Authorization", "bare", device="", token_name="bare")
    rec.me("the approver's own token afterwards", "viewer")

    rec.section = "gone"
    rec.password_signin("gone", "approver-gone", "gone")
    rec.initiate("initiate for an account to delete", "gone")
    rec.authorize("the account to delete approves", "gone", "gone", expect=200)
    delete_user(rec, port, "gone")
    rec.connect("connect after the approver is deleted", "gone")
    rec.redeem("redeem after the approver is deleted", "gone", token_name="gone-redeemed")
    if "{token:gone-redeemed}" in rec.values:
        rec.me("the token redeemed after deletion", "gone-redeemed")
    rec.initiate("initiate for an account to disable", "disabled")
    rec.authorize("the account to disable approves", "disabled", "other", expect=200)
    set_disabled(rec, port, "other", True)
    rec.redeem("redeem after the approver is disabled", "disabled", token_name="disabled-redeemed")
    if "{token:disabled-redeemed}" in rec.values:
        rec.me("the token redeemed after the approver was disabled", "disabled-redeemed")
    set_disabled(rec, port, "other", False)

    rec.section = "off"
    rec.initiate("initiate before turning off, left pending", "off-pending")
    rec.initiate("initiate before turning off, approved", "off-approved")
    rec.authorize("approve before turning off", "off-approved", "viewer", expect=200)
    set_enabled(port, rec.values["{token:admin}"], False)
    rec.action("set_enabled", enabled=False)
    rec.ask("enabled when off", "GET", "/QuickConnect/Enabled")
    rec.ask("initiate when off", "POST", "/QuickConnect/Initiate", device="off-new")
    rec.ask("initiate when off with no Authorization", "POST", "/QuickConnect/Initiate")
    rec.connect("connect a pending code when off", "off-pending", device="off-pending")
    rec.connect("connect an approved code when off", "off-approved", device="off-approved")
    rec.ask("connect an unknown secret when off", "GET", "/QuickConnect/Connect?Secret=0000deadbeef", device="off-new")
    rec.authorize("approve a pending code when off", "off-pending", "viewer")
    rec.ask("approve without a token when off", "POST", "/QuickConnect/Authorize?code={code:off-pending}", device="approver-anon")
    rec.redeem("redeem a code approved before turning off", "off-approved")
    rec.redeem("redeem a pending code when off", "off-pending")
    rec.ask("redeem an unknown secret when off", "POST", "/Users/AuthenticateWithQuickConnect", device="off-new", body='{"Secret":"0000deadbeef"}')
    set_enabled(port, rec.values["{token:admin}"], True)
    rec.action("set_enabled", enabled=True)
    rec.redeem("redeem the approved code once turned on again", "off-approved", token_name="off-approved")
    rec.connect("connect the pending code once turned on again", "off-pending")


def admin_headers(rec):
    return {"Authorization": device_header("approver-admin", rec.values["{token:admin}"])}


def user_id(rec, port, user):
    status, _, data = request(port, "GET", "/Users", admin_headers(rec))
    return next(row["Id"] for row in json.loads(data) if row["Name"] == user)


def delete_user(rec, port, user):
    status, _, data = request(port, "DELETE", "/Users/" + user_id(rec, port, user), admin_headers(rec))
    if status >= 300:
        raise SystemExit(f"deleting {user}: {status} {data[:200]!r}")
    rec.action("delete_user", user=user)


def set_disabled(rec, port, user, disabled):
    uid = user_id(rec, port, user)
    status, _, data = request(port, "GET", "/Users/" + uid, admin_headers(rec))
    policy = json.loads(data)["Policy"]
    policy["IsDisabled"] = disabled
    status, _, data = request(port, "POST", f"/Users/{uid}/Policy", {**JSON_HEADERS, **admin_headers(rec)}, json.dumps(policy))
    if status >= 300:
        raise SystemExit(f"disabling {user}: {status} {data[:200]!r}")
    rec.action("set_disabled", user=user, disabled=disabled)


def web(port, here, phase):
    env = dict(os.environ, BASE=f"http://127.0.0.1:{port}", PHASE=phase, PASSWORD=PASSWORD)
    env.setdefault("CHROMIUM_PATH", "/usr/bin/chromium")
    pw = os.environ.get("PLAYWRIGHT_DIR", os.path.expanduser("~/.cache/jellyscale/pw"))
    # The script resolves playwright-core from its working directory.
    done = subprocess.run(["node", os.path.join(here, "capture_quick_connect_web.mjs")], cwd=pw, env=env, capture_output=True, text=True, timeout=600)
    sys.stderr.write(done.stderr)
    if done.returncode != 0:
        raise SystemExit(f"capture_quick_connect_web.mjs ({phase}) failed:\n{done.stdout}")
    return json.loads(done.stdout)


def expiry(rec, port):
    """Times how long codes live. Every row is one call, at a number of seconds after the batch's Initiate."""
    rec.section = "expiry"
    names = ["pending", "early-a", "early-b", "early-c", "early-d", "late-a", "late-b"]
    codes = {}
    for name in names:
        status, _, data = request(port, "POST", "/QuickConnect/Initiate", {"Authorization": device_header("x-" + name)})
        codes[name] = json.loads(data)
    began = time.monotonic()
    rows = []

    def call(kind, name):
        auth = {"Authorization": device_header("approver-viewer", rec.values["{token:viewer}"])}
        if kind == "approve":
            status, _, data = request(port, "POST", "/QuickConnect/Authorize?code=" + codes[name]["Code"], auth)
        elif kind == "connect":
            status, _, data = request(port, "GET", "/QuickConnect/Connect?Secret=" + codes[name]["Secret"])
        else:
            status, _, data = request(port, "POST", "/Users/AuthenticateWithQuickConnect", {**JSON_HEADERS, "Authorization": device_header("x-" + name)},
                                      json.dumps({"Secret": codes[name]["Secret"]}))
        row = dict(code=name, call=kind, at_seconds=round(time.monotonic() - began), status=status)
        if kind == "connect" and status == 200:
            reply = json.loads(data)
            row["Authenticated"] = reply["Authenticated"]
            # How the server moves DateAdded, which a client may read.
            row["date_added_moved_seconds"] = round(seconds_between(codes[name]["DateAdded"], reply["DateAdded"]))
        if kind == "approve" or status != 200:
            row["body"] = data.decode("utf-8", "replace")[:100]
        rows.append(row)
        print(f"expiry: {row}", file=sys.stderr, flush=True)

    plan = [(1, "approve", n) for n in ("early-a", "early-b", "early-c", "early-d")]
    plan += [(2, "connect", "early-a"), (2, "connect", "pending")]
    plan += [(540, "approve", "late-a"), (540, "approve", "late-b"), (541, "connect", "late-a")]
    # A pending code, either side of 10 minutes after Initiate.
    plan += [(590, "connect", "pending"), (612, "connect", "pending"), (613, "approve", "pending"), (614, "redeem", "pending")]
    # A code approved at once: either side of 10 minutes after approval, then either side of 10 minutes after the
    # DateAdded the approval moved (about 11 minutes after approval). At each time the redemption comes first, so that
    # no other call about that code has made the server look for expired codes before it.
    plan += [(590, "redeem", "early-a"), (612, "redeem", "early-b"), (613, "connect", "early-b"), (615, "redeem", "early-a"),
             (650, "connect", "early-c"), (675, "connect", "early-d"), (676, "redeem", "early-d")]
    # A code approved 9 minutes after Initiate: either side of 10 minutes after that approval, and past 11.
    plan += [(540 + 590, "redeem", "late-a"), (540 + 612, "redeem", "late-b"), (540 + 613, "connect", "late-b"),
             (540 + 675, "connect", "late-b")]
    for at, kind, name in sorted(plan, key=lambda row: row[0]):
        time.sleep(max(0, began + at - time.monotonic()))
        call(kind, name)
    return rows


def seconds_between(first, second):
    def parse(value):
        stamp, _, fraction = value.rstrip("Z").partition(".")
        return time.mktime(time.strptime(stamp, "%Y-%m-%dT%H:%M:%S")) + float("0." + (fraction or "0"))
    return parse(second) - parse(first)


def main():
    out = os.path.abspath(sys.argv[1])
    here = os.path.dirname(os.path.abspath(__file__))
    name = f"jellyscale-capture-quick-connect-{os.getpid()}"
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        run("docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])

        def up():
            try:
                status, _, data = request(port, "GET", "/System/Info/Public")
                return json.loads(data) if status == 200 and b"Version" in data else None
            except (OSError, http.client.HTTPException):
                return None
        version = wait_for("jellyfin to start", up)["Version"]
        for method, path, body in (("POST", "/Startup/Configuration", '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", json.dumps({"Name": "admin", "Password": PASSWORD})),
                                   ("POST", "/Startup/RemoteAccess", '{"EnableRemoteAccess":true}'), ("POST", "/Startup/Complete", None)):
            status, _, data = request(port, method, path, JSON_HEADERS, body)
            if status >= 300:
                raise SystemExit(f"{method} {path}: {status} {data[:200]!r}")
        status, _, data = request(port, "POST", "/Users/AuthenticateByName", {**JSON_HEADERS, "Authorization": device_header("setup")},
                                  json.dumps({"Username": "admin", "Pw": PASSWORD}))
        setup_token = json.loads(data)["AccessToken"]
        for user in ("viewer", "other", "gone"):
            status, _, data = request(port, "POST", "/Users/New", {**JSON_HEADERS, "Authorization": device_header("setup", setup_token)},
                                      json.dumps({"Name": user, "Password": PASSWORD}))
            if status >= 300:
                raise SystemExit(f"creating {user}: {status} {data[:200]!r}")
        default_setting = set_enabled(port, setup_token, True)

        rec = Recorder(port)
        state_steps(rec, port)
        result = dict(image=IMAGE, server_version=version, quick_connect_available_by_default=default_setting, steps=rec.steps)

        result["web"] = dict(on=web(port, here, "on"))
        set_enabled(port, rec.values["{token:admin}"], False)
        result["web"]["off"] = web(port, here, "off")
        set_enabled(port, rec.values["{token:admin}"], True)

        rec.password_signin("viewer", "approver-viewer", "viewer")
        result["expiry"] = expiry(rec, port)

        with open(out, "w") as f:
            json.dump(result, f, indent=1)
            f.write("\n")
    finally:
        if started:
            remove_container(name)


if __name__ == "__main__":
    main()
