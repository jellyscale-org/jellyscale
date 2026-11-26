#!/usr/bin/env python3
"""Records how a real Jellyfin 12.4 and its jellyfin-web manage users (JS-033).

Usage: python3 -I test/differential/capture_users_admin.py <out.json>   (run from the repository root)
(writes <out>.json and <out>.md)
Environment: PLAYWRIGHT_DIR, a directory with playwright-core installed (default ~/.cache/jellyscale/pw);
CHROMIUM_PATH (default /usr/bin/chromium).

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with one empty library, completes the startup wizard
and asks the user routes with synthetic accounts: an administrator and two viewers, plus short-lived ones. Then
capture_users_admin_web.mjs drives the container's own jellyfin-web in Chromium. Tokens and passwords are replaced by
placeholders, and ids by the name of whom they stand for. The container is removed when the script ends.
"""
import http.client
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.parse

IMAGE = "jellyfin/jellyfin:12.4"
ADMIN_PASSWORD = "capture-admin-pass"
RIGHT = "capture-right-pass"
NEW = "capture-new-pass"
UNKNOWN_ID = "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"


def run(*cmd):
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout.strip()


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


class Redactor:
    def __init__(self):
        self.names = {}

    def learn(self, value, label):
        if value:
            self.names.setdefault(value.lower().replace("-", ""), label)

    def text(self, text):
        def replace(match):
            return self.names.get(match.group(0).lower().replace("-", ""), "<id>")

        text = re.sub(r"\b[0-9a-fA-F]{8}(?:-?[0-9a-fA-F]{4}){3}-?[0-9a-fA-F]{12}\b",
                      lambda m: m.group(0) if m.group(0).replace("-", "") in (UNKNOWN_ID, "0" * 32) else replace(m), text)
        for secret, label in ((ADMIN_PASSWORD, "<admin-password>"), (RIGHT, "<right-password>"), (NEW, "<new-password>")):
            text = text.replace(secret, label)
        return text

    def tree(self, value):
        return json.loads(self.text(json.dumps(value)))


class Jellyfin:
    def __init__(self, port):
        self.port = port
        self.redactor = Redactor()
        self.rows = []
        self.admin = None
        self.devices = 0

    def raw(self, method, path, token=None, body=None):
        device = f"capture-{(token or 'anonymous')[:8]}"
        auth = f'MediaBrowser Client="capture", Device="capture", DeviceId="{device}", Version="1"'
        if token:
            auth += f', Token="{token}"'
        headers = {"Authorization": auth}
        if body is not None:
            headers["Content-Type"] = "application/json"
            body = body if isinstance(body, str) else json.dumps(body)
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=120)
        conn.request(method, path, body=body, headers=headers)
        reply = conn.getresponse()
        text = reply.read().decode("utf-8", "replace")
        status = reply.status
        conn.close()
        parsed = None
        if text[:1] in ("{", "[", '"'):
            parsed = json.loads(text)
        return status, parsed, text

    def must(self, method, path, token=None, body=None):
        status, parsed, text = self.raw(method, path, token, body)
        if status >= 300:
            raise SystemExit(f"{method} {path}: {status} {text[:300]}")
        return parsed

    def ask(self, section, case, method, path, token=None, body=None, keep=None):
        """One recorded request. `keep` turns the parsed answer into what the capture holds."""
        status, parsed, text = self.raw(method, path, token, body)
        if parsed is not None and status < 300:
            answer = keep(parsed) if keep else summary(parsed)
        else:
            answer = text[:300]
        row = {"section": section, "case": case, "method": method, "path": self.redactor.text(path), "status": status}
        if body is not None:
            row["request"] = self.redactor.tree(short_body(body))
        if answer not in ("", None):
            row["answer"] = self.redactor.tree(answer)
        self.rows.append(row)
        return status, parsed

    def note(self, section, case, value):
        self.rows.append({"section": section, "case": case, "answer": self.redactor.tree(value)})

    def sign_in(self, name, password):
        self.devices += 1
        auth = f'MediaBrowser Client="capture", Device="capture", DeviceId="signin-{self.devices}", Version="1"'
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=120)
        conn.request("POST", "/Users/AuthenticateByName", body=json.dumps({"Username": name, "Pw": password}),
                     headers={"Authorization": auth, "Content-Type": "application/json"})
        reply = conn.getresponse()
        text = reply.read().decode()
        conn.close()
        token = json.loads(text)["AccessToken"] if reply.status == 200 else None
        return reply.status, token, text[:200] if token is None else "token issued"

    def try_sign_in(self, section, case, name, password):
        status, token, text = self.sign_in(name, password)
        self.rows.append({"section": section, "case": case, "method": "POST", "path": "/Users/AuthenticateByName",
                          "status": status, "answer": self.redactor.text(text)})
        return token

    def new_user(self, name, password=RIGHT):
        user = self.must("POST", "/Users/New", self.admin, {"Name": name, "Password": password})
        self.redactor.learn(user["Id"], f"<{name}-id>")
        return user["Id"]

    def token(self, name, password=RIGHT):
        status, token, text = self.sign_in(name, password)
        if token is None:
            raise SystemExit(f"sign-in of {name}: {status} {text}")
        return token

    def user(self, user_id):
        return self.must("GET", f"/Users/{user_id}", self.admin)

    def policy(self, user_id):
        return self.user(user_id)["Policy"]

    def set_policy(self, user_id, **changes):
        policy = self.policy(user_id)
        policy.update(changes)
        self.must("POST", f"/Users/{user_id}/Policy", self.admin, policy)

    def tokens_state(self, section, case, tokens):
        self.note(section, case, {label: self.raw("GET", "/Users/Me", token)[0] for label, token in tokens.items()})


def short_body(body):
    if isinstance(body, dict) and "Policy" in body:
        return {"(full user DTO)": True, "Name": body.get("Name"), "Policy.IsAdministrator": body["Policy"].get("IsAdministrator")}
    if isinstance(body, dict) and len(body) > 12:
        return {"(full policy, fields)": len(body), **{k: body[k] for k in ("IsAdministrator", "IsDisabled", "LoginAttemptsBeforeLockout") if k in body}}
    return body


def user_summary(user):
    policy = user.get("Policy") or {}
    return {"Name": user.get("Name"), "HasPassword": user.get("HasPassword"), "IsAdministrator": policy.get("IsAdministrator"),
            "IsDisabled": policy.get("IsDisabled"), "IsHidden": policy.get("IsHidden")}


def summary(parsed):
    if isinstance(parsed, list) and parsed and "Policy" in parsed[0]:
        return [user_summary(u) for u in parsed]
    if isinstance(parsed, dict) and "Policy" in parsed:
        return user_summary(parsed)
    return parsed


def policy_diff(before, after):
    return {k: [before.get(k), after.get(k)] for k in sorted(set(before) | set(after)) if before.get(k) != after.get(k)}


def listing(jf, admin, one, two):
    section = "list"
    hidden = jf.new_user("capture-shown")
    jf.set_policy(hidden, IsHidden=False)
    disabled = jf.new_user("capture-off")
    jf.set_policy(disabled, IsDisabled=True)
    for caller, token in (("admin", admin), ("viewer", one), ("no token", None)):
        for query in ("", "?isHidden=true", "?isHidden=false", "?isDisabled=true", "?isDisabled=false", "?isHidden=false&isDisabled=false"):
            jf.ask(section, f"{caller}", "GET", "/Users" + query, token)
    jf.ask(section, "no token", "GET", "/Users/Public")
    for user_id in (hidden, disabled):
        jf.must("DELETE", f"/Users/{user_id}", admin)


def creating(jf, admin, one):
    section = "new"
    full = {}

    def keep(user):
        full.setdefault("user", user)
        jf.redactor.learn(user["Id"], f"<{user['Name']}-id>")
        return user_summary(user)

    cases = [("viewer", one, {"Name": "capture-by-viewer", "Password": RIGHT}), ("no token", None, {"Name": "capture-anon", "Password": RIGHT}),
             ("admin, plain", admin, {"Name": "capture-made", "Password": RIGHT}),
             ("admin, same name", admin, {"Name": "capture-made", "Password": RIGHT}),
             ("admin, name taken in another case", admin, {"Name": "CAPTURE-MADE", "Password": RIGHT}),
             ("viewer, name taken in another case", one, {"Name": "CAPTURE-MADE", "Password": RIGHT}),
             ("admin, empty name", admin, {"Name": "", "Password": RIGHT}), ("viewer, empty name", one, {"Name": "", "Password": RIGHT}),
             ("admin, blank name", admin, {"Name": "   ", "Password": RIGHT}), ("admin, no Name", admin, {"Password": RIGHT}),
             ("admin, name with trailing space", admin, {"Name": "capture-trail ", "Password": RIGHT}),
             ("admin, name with a slash", admin, {"Name": "capture/slash", "Password": RIGHT}),
             ("admin, no Password field", admin, {"Name": "capture-nopass"}),
             ("admin, empty Password", admin, {"Name": "capture-emptypass", "Password": ""}),
             ("viewer, no Password field", one, {"Name": "capture-viewer-nopass"}),
             ("admin, password 'password'", admin, {"Name": "capture-common", "Password": "password"})]
    for case, token, body in cases:
        jf.ask(section, case, "POST", "/Users/New", token, body, keep=keep)
    jf.note(section, "the whole Policy of a new user", full["user"]["Policy"])
    jf.note(section, "the fields of a new user's answer", sorted(full["user"]))
    jf.try_sign_in(section, "user made without Password signs in with empty Pw", "capture-nopass", "")
    jf.try_sign_in(section, "user made without Password signs in with some Pw", "capture-nopass", "anything")
    for user in jf.must("GET", "/Users", admin):
        if user["Name"].startswith(("capture-made", "capture-trail", "capture-nopass", "capture-emptypass", "capture-common", "capture/", "  ")):
            jf.must("DELETE", f"/Users/{user['Id']}", admin)


def renaming(jf, admin, one, one_id, two_id):
    section = "update"
    victim = jf.new_user("capture-rename")
    own = jf.token("capture-rename")

    def body(user_id, **changes):
        user = jf.user(user_id)
        user.update(changes)
        return user

    def post(case, token, target, payload, query=None):
        before = jf.user(target) if target != UNKNOWN_ID else None
        jf.ask(section, case, "POST", "/Users" + (f"?userId={target}" if query is None else query), token, payload)
        if before is None:
            return
        after = jf.user(target)
        jf.note(section, case + ": what changed", {"Name": [before["Name"], after["Name"]], "Policy": policy_diff(before["Policy"], after["Policy"]),
                                                    "Configuration": policy_diff(before["Configuration"], after["Configuration"])})

    post("viewer renames themselves, full user DTO", own, victim, body(victim, Name="capture-renamed"))
    post("viewer renames another", one, two_id, body(two_id, Name="capture-stolen"))
    post("admin renames another", admin, victim, body(victim, Name="capture-by-admin"))
    post("rename to a taken name in another case", admin, victim, body(victim, Name="CAPTURE-VIEWER-ONE"))
    post("rename to own name in another case", admin, victim, body(victim, Name="CAPTURE-BY-ADMIN"))
    post("rename to empty", admin, victim, body(victim, Name=""))
    post("body is only Name", admin, victim, {"Name": "capture-only-name"})
    post("own DTO that also asks IsAdministrator", own, victim, body(victim, Policy={**jf.policy(victim), "IsAdministrator": True}))
    post("unknown id, full user DTO", admin, UNKNOWN_ID, body(victim, Name="capture-nobody"))
    jf.ask(section, "malformed id, full user DTO", "POST", "/Users?userId=not-a-guid", admin, body(victim, Name="capture-malformed"))
    jf.ask(section, "no userId", "POST", "/Users", own, body(victim, Name="capture-no-query"))
    jf.note(section, "no userId: the caller's name afterwards", jf.user(victim)["Name"])
    jf.tokens_state(section, "GET /Users/Me with the renamed user's token", {"own": own})
    jf.must("DELETE", f"/Users/{victim}", admin)


def deleting(jf, admin, admin_id, one):
    section = "delete"
    jf.ask(section, "only admin deletes themselves (the last admin)", "DELETE", f"/Users/{admin_id}", admin)
    jf.tokens_state(section, "the only admin's token after that refusal", {"admin": admin})
    # The refused delete may have signed the administrator out: sign in again for what follows.
    admin = jf.admin = jf.token("capture-admin", ADMIN_PASSWORD)
    jf.note(section, "the administrator after signing in again", user_summary(jf.user(admin_id)))
    second = jf.new_user("capture-admin-two")
    jf.set_policy(second, IsAdministrator=True)
    second_token = jf.token("capture-admin-two")
    jf.ask(section, "viewer deletes an administrator", "DELETE", f"/Users/{second}", one)
    jf.ask(section, "one of two administrators deletes themselves", "DELETE", f"/Users/{second}", second_token)
    jf.tokens_state(section, "their token afterwards", {"deleted": second_token})

    spare = jf.new_user("capture-spare-admin")
    jf.set_policy(spare, IsAdministrator=True, IsDisabled=False)
    jf.ask(section, "disable the second administrator through Policy", "POST", f"/Users/{spare}/Policy", admin, {**jf.policy(spare), "IsDisabled": True})
    jf.must("DELETE", f"/Users/{spare}", admin)

    gone = jf.new_user("capture-gone")
    token = jf.token("capture-gone")
    jf.ask(section, "admin deletes a viewer", "DELETE", f"/Users/{gone}", admin)
    for label, bearer in (("deleted user's token", token), ("made-up token", "00000000000000000000000000000000")):
        for path in ("/Users/Me", "/Items", "/System/Info", "/Users/Public"):
            jf.ask(section, label, "GET", path, bearer)
    jf.try_sign_in(section, "deleted name, its old password", "capture-gone", RIGHT)
    jf.try_sign_in(section, "never-existing name", "capture-never", RIGHT)
    jf.try_sign_in(section, "existing name, wrong password", "capture-viewer-two", "wrong")
    for label, target in (("deleted id", gone), ("unknown id", UNKNOWN_ID), ("malformed id", "not-a-guid"), ("all-zero id", "0" * 32)):
        jf.ask(section, f"GET {label}", "GET", f"/Users/{target}", admin)
        jf.ask(section, f"admin, {label}", "DELETE", f"/Users/{target}", admin)
        jf.ask(section, f"viewer, {label}", "DELETE", f"/Users/{target}", one)
    again = jf.new_user("capture-gone")
    jf.tokens_state(section, "old token after the name is made again", {"deleted": token})
    jf.must("DELETE", f"/Users/{again}", admin)
    return admin


def policies(jf, admin, admin_id, one, one_id):
    section = "policy"
    jf.note(section, "the administrator's whole Policy", jf.policy(admin_id))
    jf.note(section, "a viewer's whole Policy", jf.policy(one_id))
    target = jf.new_user("capture-policy")

    def post(case, body, user_id=target, token=admin):
        before = jf.policy(user_id)
        status, _ = jf.ask(section, case, "POST", f"/Users/{user_id}/Policy", token, body)
        # An administrator who demotes or disables themselves can no longer read; another one reads for them.
        after = jf.policy(user_id)
        jf.note(section, case + ": Policy changes [before, after]", policy_diff(before, after))
        return status

    post("GET's own Policy posted back", jf.policy(target))
    required = {k: jf.policy(target)[k] for k in ("AuthenticationProviderId", "PasswordResetProviderId")}
    post("empty object", {})
    post("only IsDisabled=false", {"IsDisabled": False})
    jf.set_policy(target, IsHidden=False, EnableContentDownloading=False, LoginAttemptsBeforeLockout=3, EnableAllFolders=False)
    post("only the two provider ids", required)
    for field in ("AuthenticationProviderId", "PasswordResetProviderId"):
        post(f"full body without {field}", {k: v for k, v in jf.policy(target).items() if k != field})
    for field, value in (("IsAdministrator", True), ("IsDisabled", True), ("IsHidden", False), ("EnableContentDownloading", False),
                         ("LoginAttemptsBeforeLockout", 3), ("InvalidLoginAttemptCount", 2), ("EnableAllFolders", False),
                         ("EnableMediaPlayback", False)):
        jf.set_policy(target, IsAdministrator=False, IsDisabled=False)
        jf.set_policy(target, **{field: value})
        post(f"full body without {field} (stored {json.dumps(value)})", {k: v for k, v in jf.policy(target).items() if k != field})
        jf.set_policy(target, IsAdministrator=False, IsDisabled=False)
    for field in ("IsAdministrator", "IsDisabled", "LoginAttemptsBeforeLockout"):
        post(f"full body with {field}=null", {**jf.policy(target), field: None})
    for value in (-5, -1, 0, 1, 5, 1000):
        post(f"LoginAttemptsBeforeLockout={value}", {**jf.policy(target), "LoginAttemptsBeforeLockout": value})
    post("unknown field", {**jf.policy(target), "NotAField": True})
    jf.ask(section, "viewer posts their own Policy", "POST", f"/Users/{one_id}/Policy", one, jf.policy(one_id))
    jf.ask(section, "viewer posts another's Policy", "POST", f"/Users/{target}/Policy", one, jf.policy(target))
    jf.ask(section, "unknown id", "POST", f"/Users/{UNKNOWN_ID}/Policy", admin, jf.policy(target))
    jf.ask(section, "malformed id", "POST", "/Users/not-a-guid/Policy", admin, jf.policy(target))
    post("last administrator demotes themselves", {**jf.policy(admin_id), "IsAdministrator": False}, user_id=admin_id)
    post("last administrator posts a body without IsAdministrator", {k: v for k, v in jf.policy(admin_id).items() if k != "IsAdministrator"}, user_id=admin_id)
    post("last administrator posts only the provider ids", required, user_id=admin_id)
    post("administrator disables themselves", {**jf.policy(admin_id), "IsDisabled": True}, user_id=admin_id)

    second = jf.new_user("capture-admin-two")
    jf.set_policy(second, IsAdministrator=True)
    second_token = jf.token("capture-admin-two")
    post("one of two administrators disables themselves", {**jf.policy(second), "IsDisabled": True}, user_id=second, token=second_token)
    post("one of two administrators demotes and disables themselves at once", {**jf.policy(second), "IsAdministrator": False, "IsDisabled": True},
         user_id=second, token=second_token)
    jf.set_policy(second, IsAdministrator=True, IsDisabled=False)
    second_token = jf.token("capture-admin-two")
    post("one of two administrators demotes themselves", {**jf.policy(second), "IsAdministrator": False}, user_id=second, token=second_token)
    jf.ask(section, "the demoted token on an administrator route", "GET", "/Auth/Providers", second_token)
    jf.must("DELETE", f"/Users/{second}", admin)
    jf.must("DELETE", f"/Users/{target}", admin)


def lockout(jf, admin):
    section = "lockout"
    for label, limit in (("limit 3", 3), ("limit -1", -1), ("limit 0", 0)):
        target = jf.new_user("capture-lock")
        jf.set_policy(target, LoginAttemptsBeforeLockout=limit)
        held = jf.token("capture-lock")
        trace = []
        for attempt in range(1, 7):
            status = jf.sign_in("capture-lock", "wrong")[0]
            policy = jf.policy(target)
            trace.append({"wrong": attempt, "status": status, "InvalidLoginAttemptCount": policy["InvalidLoginAttemptCount"], "IsDisabled": policy["IsDisabled"]})
        jf.note(section, f"{label}: wrong passwords", trace)
        jf.try_sign_in(section, f"{label}: right password afterwards", "capture-lock", RIGHT)
        jf.note(section, f"{label}: counter after that", {k: jf.policy(target)[k] for k in ("InvalidLoginAttemptCount", "IsDisabled", "LoginAttemptsBeforeLockout")})
        jf.tokens_state(section, f"{label}: the token held from before", {"held": held})
        if jf.policy(target)["IsDisabled"]:
            jf.set_policy(target, IsDisabled=False)
            jf.note(section, f"{label}: counter after an administrator enables", {k: jf.policy(target)[k] for k in ("InvalidLoginAttemptCount", "IsDisabled")})
            jf.sign_in("capture-lock", "wrong")
            jf.note(section, f"{label}: after enable, one more wrong password", {k: jf.policy(target)[k] for k in ("InvalidLoginAttemptCount", "IsDisabled")})
            jf.set_policy(target, IsDisabled=False)
            jf.try_sign_in(section, f"{label}: right password after enable", "capture-lock", RIGHT)
            jf.note(section, f"{label}: counter after that sign-in", {k: jf.policy(target)[k] for k in ("InvalidLoginAttemptCount", "IsDisabled")})
        jf.must("DELETE", f"/Users/{target}", admin)

    second = jf.new_user("capture-lock-admin")
    jf.set_policy(second, IsAdministrator=True, LoginAttemptsBeforeLockout=3)
    statuses = [jf.sign_in("capture-lock-admin", "wrong")[0] for _ in range(4)]
    jf.note(section, "a second administrator, limit 3, four wrong passwords",
            {"statuses": statuses, **{k: jf.policy(second)[k] for k in ("IsAdministrator", "InvalidLoginAttemptCount", "IsDisabled")}})
    jf.must("DELETE", f"/Users/{second}", admin)


def disabling(jf, admin):
    section = "disable"
    target = jf.new_user("capture-disable")
    tokens = {"first": jf.token("capture-disable"), "second": jf.token("capture-disable")}
    jf.tokens_state(section, "tokens before", tokens)
    jf.set_policy(target, IsDisabled=True)
    for label, token in tokens.items():
        for path in ("/Users/Me", "/Items"):
            jf.ask(section, f"disabled, {label} token", "GET", path, token)
    jf.try_sign_in(section, "disabled, right password", "capture-disable", RIGHT)
    jf.try_sign_in(section, "disabled, wrong password", "capture-disable", "wrong")
    jf.try_sign_in(section, "enabled user, wrong password", "capture-viewer-two", "wrong")
    jf.try_sign_in(section, "unknown name", "capture-never", "wrong")
    jf.note(section, "counter while disabled", jf.policy(target)["InvalidLoginAttemptCount"])
    jf.set_policy(target, IsDisabled=False)
    jf.tokens_state(section, "old tokens after re-enable", tokens)
    jf.try_sign_in(section, "re-enabled, right password", "capture-disable", RIGHT)
    unused = {"never used while disabled": jf.token("capture-disable")}
    jf.set_policy(target, IsDisabled=True)
    jf.set_policy(target, IsDisabled=False)
    jf.tokens_state(section, "a token not presented while disabled, after re-enable", unused)
    jf.must("DELETE", f"/Users/{target}", admin)


def passwords(jf, admin, one):
    section = "password"
    target = jf.new_user("capture-pass")
    state = {"password": RIGHT}

    def sessions():
        return {"caller": jf.token("capture-pass", state["password"]), "other": jf.token("capture-pass", state["password"])}

    def has_password():
        return jf.user(target)["HasPassword"]

    def change(case, body, by_admin=False, becomes=None, token=None, user_id=target):
        tokens = sessions()
        bearer = admin if by_admin else token or tokens["caller"]
        status, _ = jf.ask(section, case, "POST", f"/Users/Password?userId={user_id}", bearer, body)
        if status < 300 and becomes is not None:
            state["password"] = becomes
        jf.tokens_state(section, case + ": the user's tokens afterwards", tokens)
        jf.note(section, case + ": HasPassword", has_password())

    change("own, wrong CurrentPw", {"CurrentPw": "wrong", "NewPw": NEW})
    change("own, empty CurrentPw", {"CurrentPw": "", "NewPw": NEW})
    change("own, no CurrentPw", {"NewPw": NEW})
    change("own, right CurrentPw", {"CurrentPw": RIGHT, "NewPw": NEW}, becomes=NEW)
    change("admin for another, no CurrentPw", {"NewPw": RIGHT}, by_admin=True, becomes=RIGHT)
    change("admin for another, wrong CurrentPw", {"CurrentPw": "wrong", "NewPw": NEW}, by_admin=True, becomes=NEW)
    change("viewer for another", {"CurrentPw": NEW, "NewPw": RIGHT}, token=one)
    change("own, right CurrentPw, new password 'password'", {"CurrentPw": NEW, "NewPw": "password"}, becomes="password")
    change("own, right CurrentPw, one letter", {"CurrentPw": "password", "NewPw": "a"}, becomes="a")
    change("admin sets it back", {"NewPw": RIGHT}, by_admin=True, becomes=RIGHT)
    change("own, ResetPassword without CurrentPw", {"ResetPassword": True}, becomes="")
    jf.try_sign_in(section, "after own reset without CurrentPw, the old password", "capture-pass", RIGHT)
    change("admin sets a password again", {"NewPw": RIGHT}, by_admin=True, becomes=RIGHT)
    change("own, ResetPassword with wrong CurrentPw", {"ResetPassword": True, "CurrentPw": "wrong"}, becomes="")
    change("admin sets a password once more", {"NewPw": RIGHT}, by_admin=True, becomes=RIGHT)
    change("own, ResetPassword with right CurrentPw", {"ResetPassword": True, "CurrentPw": RIGHT}, becomes="")
    jf.try_sign_in(section, "after reset, empty password", "capture-pass", "")
    jf.try_sign_in(section, "after reset, the old password", "capture-pass", RIGHT)
    change("no password: own, wrong CurrentPw", {"CurrentPw": "wrong", "NewPw": NEW})
    change("no password: own, no CurrentPw", {"NewPw": NEW}, becomes=NEW)
    change("admin ResetPassword for another", {"ResetPassword": True}, by_admin=True, becomes="")
    change("no password: own, empty CurrentPw", {"CurrentPw": "", "NewPw": NEW}, becomes=NEW)
    change("own, right CurrentPw, empty NewPw", {"CurrentPw": NEW, "NewPw": ""}, becomes="")
    change("admin, empty body", {}, by_admin=True, becomes="")
    jf.try_sign_in(section, "after the admin's empty body, empty password", "capture-pass", "")
    change("unknown id", {"NewPw": NEW}, by_admin=True, user_id=UNKNOWN_ID)
    change("malformed id", {"NewPw": NEW}, by_admin=True, user_id="not-a-guid")

    # No userId, and the administrator on their own password (a second administrator, so the main one is left alone).
    second = jf.new_user("capture-admin-two")
    jf.set_policy(second, IsAdministrator=True)
    token = jf.token("capture-admin-two")
    jf.ask(section, "admin for themselves, no CurrentPw", "POST", f"/Users/Password?userId={second}", token, {"NewPw": NEW})
    jf.ask(section, "admin for themselves, wrong CurrentPw", "POST", f"/Users/Password?userId={second}", token, {"CurrentPw": "wrong", "NewPw": NEW})
    jf.ask(section, "no userId, right CurrentPw", "POST", "/Users/Password", jf.token("capture-admin-two", jf_password(jf, "capture-admin-two")),
           {"CurrentPw": jf_password(jf, "capture-admin-two"), "NewPw": NEW})
    jf.must("DELETE", f"/Users/{second}", admin)
    jf.must("DELETE", f"/Users/{target}", admin)


def jf_password(jf, name):
    for password in (RIGHT, NEW):
        if jf.sign_in(name, password)[0] == 200:
            return password
    raise SystemExit(f"no known password of {name}")


def providers(jf, admin, one):
    for caller, token in (("admin", admin), ("viewer", one), ("no token", None)):
        for path in ("/Auth/Providers", "/Auth/PasswordResetProviders"):
            jf.ask("providers", caller, "GET", path, token)


def deprecated_routes(openapi):
    every, old = [], []
    for path, operations in openapi["paths"].items():
        for method, operation in operations.items():
            every.append(f"{method.upper()} {path}")
            if operation.get("deprecated"):
                old.append(f"{method.upper()} {path}")
    return every, old


def match_route(method, path, routes):
    """The OpenAPI template a concrete request matches; a literal segment beats a placeholder."""
    parts = path.lower().strip("/").split("/")
    found = []
    for route in routes:
        verb, template = route.split(" ", 1)
        wanted = template.lower().strip("/").split("/")
        if verb == method and len(wanted) == len(parts) and all(w.startswith("{") or w == p for w, p in zip(wanted, parts)):
            found.append((sum(w.startswith("{") for w in wanted), route))
    return min(found)[1] if found else None


# Read from the rows below. A rerun does not rewrite these sentences: check them against its rows.
SUMMARY = """## What stands out

- `GET /Users` answers a viewer the same full list as an administrator, and `isHidden`/`isDisabled` filter it for both.
- A Policy body needs only `AuthenticationProviderId` and `PasswordResetProviderId` (400 without either). Every other missing field takes
  the class's default, not the stored value: a missing `IsAdministrator` demotes (403 for the last administrator), a missing `IsDisabled` enables.
- `LoginAttemptsBeforeLockout` and `InvalidLoginAttemptCount` are the Policy's lockout fields. -1 never locks; 0 is stored as 3 for a
  viewer. At a limit of 3 the third wrong password disables the account (the counter goes from 2 to 4), an administrator's included.
  Enabling does not reset the counter, so one more wrong password disables again; a successful sign-in resets it.
- Disabling ends the user's tokens for good: they stay 401 after re-enabling, whether or not they were presented meanwhile.
- `DELETE` of the last administrator by themselves is 400, but has already revoked their token.
- A password change keeps the caller's token and ends the user's others; an administrator changing another's ends all of that user's.
  `ResetPassword` needs no `CurrentPw`, even for one's own, and ends no token. `HasPassword` stays true with an empty password.
- jellyfin-web's password pages call `POST /Users/{userId}/Password`, which is not in the OpenAPI document: in Jellyfin 12.4's source
  (`Jellyfin.Api/Controllers/UserController.cs`, `UpdateUserPasswordLegacy`) it is `[Obsolete]` and hidden from the document.
- Save with no change posts GET's own values on the profile and parental-control pages. The access page differs in two: it posts
  `BlockedMediaFolders` and `BlockedChannels` as `null` where GET answers `[]`.
- The new-user page posts only `Name` and `Password` to `/Users/New`, then a full Policy. With its boxes left alone that Policy changes
  `EnableAllFolders` and `EnableAllChannels` from true to false; ticking a library puts its id in `EnabledFolders`.
- A refused Policy Save (400 or 403) on the profile, access or parental-control page shows no message and leaves the spinner. So does a
  refused Reset Password. A refused delete, user list and Quick Connect authorize show nothing. A refused password change always says the
  user name or password is invalid. A refused Policy after New leaves the user made, with a generic error and the spinner.
"""


def markdown(result):
    lines = ["# How Jellyfin 12.4 manages users (JS-033)\n",
             f"Observed with `{result['image']}` (server {result['server_version']}), a disposable container on a Docker-assigned port with synthetic "
             "accounts, by `test/differential/capture_users_admin.py` (API) and `capture_users_admin_web.mjs` (the container's jellyfin-web in Chromium). "
             "Rerun from the repository root: `python3 -I test/differential/capture_users_admin.py compat/captures/users-admin-12.4.json`. "
             "Tokens and passwords are placeholders, ids are named after whom they stand for. The JSON holds the same rows in full.\n"]
    lines.append(SUMMARY)
    section = None
    for row in result["api"]:
        if row["section"] != section:
            section = row["section"]
            lines.append(f"\n## API: {section}\n")
        compact = lambda value: json.dumps(value, separators=(",", ":")).replace("|", "\\|")
        text = f"- {row['case']}"
        if "method" in row:
            text += f": `{row['method']} {row['path']}`"
            if "request" in row:
                text += f" `{compact(row['request'])}`"
            text += f" → **{row['status']}**"
        if "answer" in row:
            answer = compact(row["answer"])
            text += f" `{answer if len(answer) <= 600 else answer[:600] + '…'}`"
        lines.append(text)
    web = result["web"]
    lines.append("\n## jellyfin-web: what each page sends\n")
    lines.append("Each load builds the app again, so it holds the app's own start-up requests too. `/System/Info*`, `/System/Endpoint`, "
                 "`/Playback/BitrateTest` and `/Sessions/Capabilities/Full` are left out here and kept in the JSON. A request is listed once. "
                 "Each page and each refusal has its own browser context: the app caches answers, so a later load in one context "
                 "can show a page without asking again.\n")
    for page in web["pages"]:
        lines.append(f"\n### {page['page']} ({page['as']})\n")
        for step in page["steps"]:
            lines.append(f"- {step['step']}:")
            for request in step["requests"]:
                if re.match(r"/(System/(Info|Endpoint)|Playback/BitrateTest|Sessions/Capabilities)", request["path"]):
                    continue
                body = f" body `{request['body']}`" if request.get("body") else ""
                lines.append(f"  - `{request['method']} {request['path']}` → {request['status']}{body}")
            if step.get("shows") is not None:
                lines.append(f"  - shows: `{json.dumps(step['shows'])}`")
    lines.append("\n## jellyfin-web: answers of 400 and 403 (given by the capture in the server's place)\n")
    for row in web["refusals"]:
        lines.append(f"- {row['page']}, {row['action']}, `{row['refused']}` → {row['status']}: shows `{json.dumps(row['shows'])}`; "
                     f"ends at `{row['hash']}`; requests after the refusal: {', '.join(f'`{r}`' for r in row['after']) or 'none'}"
                     + (f"; user requests of the load: {', '.join(f'`{r}`' for r in row['user_requests'])}" if row.get("user_requests") else "")
                     + (f"; page errors: `{json.dumps(row['errors'])}`" if row["errors"] else ""))
    routes = result["routes"]
    lines.append("\n## Obsolete routes\n")
    lines.append(f"The server's `/api-docs/openapi.json` marks {len(routes['deprecated_in_openapi'])} operations deprecated. "
                 f"Requests of these pages that match one: {', '.join(f'`{r}`' for r in routes['web_calls_deprecated']) or 'none'}. "
                 f"Requests that match no operation in the document: {', '.join(f'`{r}`' for r in routes['web_calls_not_in_openapi']) or 'none'}.")
    lines.append("")
    return "\n".join(lines)


def main():
    out = os.path.abspath(sys.argv[1])
    here = os.path.dirname(os.path.abspath(__file__))
    name = f"jellyscale-capture-users-admin-{os.getpid()}"
    work = tempfile.mkdtemp(prefix="capture-users-admin-")
    os.makedirs(os.path.join(work, "movies"))
    os.chmod(work, 0o755)
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        run("docker", "run", "-d", "--rm", "--name", name, "-v", f"{work}:/media:ro", "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])
        jf = Jellyfin(port)

        def up():
            try:
                status, parsed, _ = jf.raw("GET", "/System/Info/Public")
                return parsed if status == 200 and parsed and "Version" in parsed else None
            except (OSError, http.client.HTTPException, ValueError):
                return None
        version = wait_for("jellyfin to start", up)["Version"]
        for method, path, body in (("POST", "/Startup/Configuration", {"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", {"Name": "capture-admin", "Password": ADMIN_PASSWORD}),
                                   ("POST", "/Startup/RemoteAccess", {"EnableRemoteAccess": True}), ("POST", "/Startup/Complete", None)):
            jf.must(method, path, None, body)
        admin = jf.token("capture-admin", ADMIN_PASSWORD)
        jf.admin = admin
        admin_id = jf.must("GET", "/Users/Me", admin)["Id"]
        jf.redactor.learn(admin_id, "<capture-admin-id>")
        jf.redactor.learn(jf.must("GET", "/System/Info/Public")["Id"], "<server-id>")
        jf.must("POST", "/Library/VirtualFolders?refreshLibrary=false&name=Movies&collectionType=movies", admin,
                {"LibraryOptions": {"PathInfos": [{"Path": "/media/movies"}]}})
        one_id, two_id = jf.new_user("capture-viewer-one"), jf.new_user("capture-viewer-two")
        one = jf.token("capture-viewer-one")

        listing(jf, admin, one, two_id)
        creating(jf, admin, one)
        renaming(jf, admin, one, one_id, two_id)
        admin = deleting(jf, admin, admin_id, one)
        policies(jf, admin, admin_id, one, one_id)
        lockout(jf, admin)
        disabling(jf, admin)
        passwords(jf, admin, one)
        providers(jf, admin, one)

        env = dict(os.environ, BASE=f"http://127.0.0.1:{port}", ADMIN_NAME="capture-admin", ADMIN_PASSWORD=ADMIN_PASSWORD,
                   VIEWER_NAME="capture-viewer-one", VIEWER_ID=one_id, VIEWER_PASSWORD=RIGHT, TARGET_ID=two_id, NEW_PASSWORD=NEW)
        env.setdefault("CHROMIUM_PATH", "/usr/bin/chromium")
        playwright = os.environ.get("PLAYWRIGHT_DIR", os.path.expanduser("~/.cache/jellyscale/pw"))
        web = subprocess.run(["node", os.path.join(here, "capture_users_admin_web.mjs")], cwd=playwright, env=env, capture_output=True, text=True, timeout=1200)
        sys.stderr.write(web.stderr)
        if web.returncode != 0:
            raise SystemExit(f"capture_users_admin_web.mjs failed:\n{web.stdout[:2000]}")
        web = json.loads(web.stdout)
        for user in jf.must("GET", "/Users", admin):
            jf.redactor.learn(user["Id"], f"<{user['Name']}-id>")

        every, old = deprecated_routes(jf.must("GET", "/api-docs/openapi.json", admin))
        sent = [r for page in web["pages"] for step in page["steps"] for r in step["requests"]]
        called, unlisted = set(), set()
        for request in sent:
            path = urllib.parse.urlsplit(request["path"]).path
            route = match_route(request["method"], path, every)
            if route is None:
                unlisted.add(f"{request['method']} {jf.redactor.text(path)}")
            elif route in old:
                called.add(route)

        # Ids that only jellyfin-web saw (devices, libraries) become <id>.
        result = {"image": IMAGE, "server_version": version, "api": jf.rows, "web": jf.redactor.tree(web),
                  "routes": {"deprecated_in_openapi": sorted(old), "web_calls_deprecated": sorted(called), "web_calls_not_in_openapi": sorted(unlisted)}}
        with open(out, "w") as f:
            json.dump(result, f, indent=1)
            f.write("\n")
        with open(re.sub(r"\.json$", ".md", out), "w") as f:
            f.write(markdown(result))
    finally:
        if started:
            remove_container(name)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
