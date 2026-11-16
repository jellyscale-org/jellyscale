#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 does with the user Configuration routes (JS-116).

Usage: python3 -I test/differential/capture_user_configuration.py <out.json>

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with no library, completes its startup
wizard, and for each case makes three fresh users (U and O, who are not administrators, and A, who is),
sends the case's steps and request, and records the answer and what GET /Users/Me (each user's own token) and
GET /Users/{id} (by A) hold afterwards. It also records GET /Localization/Cultures with and without a token.
The Go replay (internal/api/user_configuration_integration_test.go) sends the same steps to jellyscale and
compares. Synthetic names only; the container is removed when the script ends.
"""
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

IMAGE = "jellyfin/jellyfin:12.4"
PASSWORD = "capture-pass"

# The 16 fields of UserConfiguration, in the order the schema lists them.
FIELDS = [
    "AudioLanguagePreference", "PlayDefaultAudioTrack", "SubtitleLanguagePreference", "DisplayMissingEpisodes",
    "GroupedFolders", "SubtitleMode", "DisplayCollectionsView", "EnableLocalPassword", "OrderedViews",
    "LatestItemsExcludes", "MyMediaExcludes", "HidePlayedInLatest", "RememberAudioSelections",
    "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay", "CastReceiverId",
]

I1, I2, I3, I4 = ("1a2b3c4d5e6f40718293a4b5c6d7e8f%d" % n for n in (1, 2, 3, 4))
D1 = "1A2B3C4D-5E6F-4071-8293-A4B5C6D7E8F5"  # dashed, upper case


def full(n):
    """All 16 fields changed from a new user's. n picks one of two bodies that differ in every field."""
    if n == 0:
        return {
            "AudioLanguagePreference": "fra", "PlayDefaultAudioTrack": False, "SubtitleLanguagePreference": "deu",
            "DisplayMissingEpisodes": True, "GroupedFolders": [I1], "SubtitleMode": "Always",
            "DisplayCollectionsView": True, "EnableLocalPassword": True, "OrderedViews": [I2, I3],
            "LatestItemsExcludes": [I1, I2, I3], "MyMediaExcludes": [I4], "HidePlayedInLatest": False,
            "RememberAudioSelections": False, "RememberSubtitleSelections": False,
            "EnableNextEpisodeAutoPlay": False, "CastReceiverId": "ABCD1234",
        }
    return {
        "AudioLanguagePreference": "spa", "PlayDefaultAudioTrack": False, "SubtitleLanguagePreference": "eng",
        "DisplayMissingEpisodes": True, "GroupedFolders": [I4, I3], "SubtitleMode": "OnlyForced",
        "DisplayCollectionsView": True, "EnableLocalPassword": True, "OrderedViews": [I1],
        "LatestItemsExcludes": [I4], "MyMediaExcludes": [I1, I2, I3], "HidePlayedInLatest": False,
        "RememberAudioSelections": False, "RememberSubtitleSelections": False,
        "EnableNextEpisodeAutoPlay": False, "CastReceiverId": "WXYZ9876",
    }


def j(value):
    return json.dumps(value, separators=(",", ":"))


def step(actor, route, target, body, content_type="application/json"):
    return {"actor": actor, "route": route, "target": target, "body": body, "contentType": content_type}


SAVED = [step("U", "path", "U", j(full(0))), step("O", "path", "O", j(full(1)))]


def cases():
    out = []

    def case(name, request, before=SAVED, note=""):
        out.append({"name": name, "note": note, "before": before, "request": request})

    # A new user who has saved nothing.
    case("new user, nothing saved", None, before=[])
    for route in ("path", "query", "none"):
        case(f"full body, route={route}", step("U", route, "U", j(full(0))), before=[])
        # All 16 fields changed, then a body with only SubtitleMode.
        case(f"one field after all 16 changed, route={route}", step("U", route, "U", j({"SubtitleMode": "Smart"})))
        case(f"two bodies of one field each, route={route}", step("U", route, "U", j({"PlayDefaultAudioTrack": True})),
             before=SAVED + [step("U", route, "U", j({"SubtitleLanguagePreference": "ita"}))])
        case(f"unknown field beside known ones, route={route}",
             step("U", route, "U", j({"NoSuchField": "secret-value", "SubtitleMode": "Smart", "HidePlayedInLatest": True})))
        for mode in ("Default", "Always", "OnlyForced", "None", "Smart"):
            case(f"SubtitleMode {mode}, route={route}", step("U", route, "U", j({**full(0), "SubtitleMode": mode})), before=[])
        for what, value in (("unlisted word", "Sometimes"), ("lower case", "always"), ("number", 3), ("number zero", 0), ("empty", ""), ("null", None)):
            case(f"SubtitleMode outside the enum: {what}, route={route}", step("U", route, "U", j({**full(1), "SubtitleMode": value})))
        case(f"boolean sent as a string, route={route}", step("U", route, "U", j({**full(1), "PlayDefaultAudioTrack": "true"})))
        case(f"list entry that is not an id, route={route}", step("U", route, "U", j({**full(1), "GroupedFolders": ["not-an-id"]})))
        case(f"list sent as a string, route={route}", step("U", route, "U", j({**full(1), "OrderedViews": I1})))
        case(f"language set then cleared with an empty string, route={route}", step("U", route, "U", j({"AudioLanguagePreference": "", "SubtitleLanguagePreference": ""})))
        case(f"language set then cleared with null, route={route}", step("U", route, "U", j({"AudioLanguagePreference": None, "SubtitleLanguagePreference": None})))
        case(f"full body with the two languages cleared by null, route={route}", step("U", route, "U", j({**full(1), "AudioLanguagePreference": None, "SubtitleLanguagePreference": None})))
        case(f"AudioLanguagePreference OriginalLanguage, route={route}", step("U", route, "U", j({**full(1), "AudioLanguagePreference": "OriginalLanguage"})), before=[])
        case(f"body that is not JSON, route={route}", step("U", route, "U", "this is not json"))
        case(f"empty body, route={route}", step("U", route, "U", ""))
        case(f"JSON that is not an object, route={route}", step("U", route, "U", "[1,2]"))
        case(f"JSON null, route={route}", step("U", route, "U", "null"))
        case(f"body over 64 KB (ids in a list), route={route}",
             step("U", route, "U", j({**full(1), "LatestItemsExcludes": [str(uuid.UUID(int=n)) for n in range(2000)]})))
        case(f"body over 64 KB (an unknown field), route={route}", step("U", route, "U", j({**full(1), "Padding": "x" * 70000})))
        case(f"body over 64 KB (bytes after the object), route={route}", step("U", route, "U", j(full(1)) + " " * 70000))
        case(f"20 ids in each list, route={route}",
             step("U", route, "U", j({**full(1), **{k: [str(uuid.UUID(int=0x1000 * (i + 1) + n)) for n in range(20)]
                                                       for i, k in enumerate(("GroupedFolders", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes"))}})), before=[])
        case(f"ids in dashed upper case and out of order, route={route}",
             step("U", route, "U", j({**full(1), "GroupedFolders": [D1, I4, I2, I3, I1], "OrderedViews": [I3, I3]})), before=[])
        case(f"no token, route={route}", step("anonymous", route, "U", j(full(1))))
        case(f"a token no device holds, route={route}", step("badtoken", route, "U", j(full(1))))
    # Naming a user needs a route that carries an id: POST /Users/Configuration with no userId names nobody.
    for route in ("path", "query"):
        case(f"another user's id as a non-administrator, route={route}", step("U", route, "O", j(full(1))))
        case(f"administrator names another user, route={route}", step("A", route, "U", j(full(1))))
        case(f"administrator names themselves, route={route}", step("A", route, "A", j(full(1))))
        case(f"unknown id as an administrator, route={route}", step("A", route, "unknown", j(full(1))))
        case(f"unknown id as a non-administrator, route={route}", step("U", route, "unknown", j(full(1))))
        case(f"malformed id as an administrator, route={route}", step("A", route, "malformed", j(full(1))))
        case(f"malformed id as a non-administrator, route={route}", step("U", route, "malformed", j(full(1))))
    # A user naming themselves, written four ways, saves as one naming nobody.
    for spelling in ("selfDashed", "selfCompact", "selfDashedUpper", "selfCompactUpper"):
        case(f"naming oneself, {spelling}, in the path", step("U", "path", spelling, j(full(1))), before=[])
        case(f"naming oneself, {spelling}, in the query", step("U", "query", spelling, j(full(1))), before=[])
    case("administrator names another user in compact upper case, in the path", step("A", "path", "UcompactUpper", j(full(1))))
    case("administrator names another user in dashed form, in the query", step("A", "query", "Udashed", j(full(1))))
    return out


class Jellyfin:
    def __init__(self, base):
        self.base = base

    def call(self, method, path, token=None, body=None, content_type="application/json"):
        data = None if body is None else body.encode()
        auth = 'MediaBrowser Client="capture", Device="capture", DeviceId="capture-device", Version="1"'
        if token:
            auth += f', Token="{token}"'
        request = urllib.request.Request(self.base + path, data=data, method=method)
        request.add_header("Authorization", auth)
        if data is not None:
            request.add_header("Content-Type", content_type)
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                status, raw, ctype = response.status, response.read(), response.headers.get("Content-Type")
        except urllib.error.HTTPError as error:
            status, raw, ctype = error.code, error.read(), error.headers.get("Content-Type")
        text = raw.decode("utf-8", "replace")
        try:
            parsed = json.loads(text) if text.strip() else None
        except ValueError:
            parsed = None
        return {"status": status, "contentType": ctype, "text": text, "json": parsed}


def wait_ready(jf):
    for _ in range(120):
        try:
            if "StartupWizardCompleted" in (jf.call("GET", "/System/Info/Public")["json"] or {}):
                time.sleep(3)  # The wizard routes come up a moment after the public info.
                return
        except OSError:
            pass
        time.sleep(1)
    raise SystemExit("jellyfin did not start")


def wizard(jf, admin_name):
    jf.call("POST", "/Startup/Configuration", body=j({"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}))
    jf.call("GET", "/Startup/User")
    jf.call("POST", "/Startup/User", body=j({"Name": admin_name, "Password": PASSWORD}))
    jf.call("POST", "/Startup/RemoteAccess", body=j({"EnableRemoteAccess": True}))
    jf.call("POST", "/Startup/Complete")


def sign_in(jf, name):
    reply = jf.call("POST", "/Users/AuthenticateByName", body=j({"Username": name, "Pw": PASSWORD}))
    assert reply["status"] == 200, reply
    return reply["json"]["AccessToken"], reply["json"]["User"]["Id"], reply["json"]["User"]


def make_users(jf, admin_token, tag):
    """U and O are not administrators; A is made an administrator, so no case starts from another's settings."""
    users = {}
    for who in ("U", "O", "A"):
        name = f"{tag}-{who}"
        made = jf.call("POST", "/Users/New", admin_token, j({"Name": name, "Password": PASSWORD}))
        assert made["status"] == 200, made
        if who == "A":
            policy = made["json"]["Policy"]
            policy["IsAdministrator"] = True
            assert jf.call("POST", f"/Users/{made['json']['Id']}/Policy", admin_token, j(policy))["status"] == 204
        token, uid, _ = sign_in(jf, name)
        users[who] = {"id": uid, "token": token}
    return users


def target_id(users, target):
    ids = {k: v["id"] for k, v in users.items()}
    dashed = lambda h: str(uuid.UUID(h))
    return {
        "U": ids["U"], "O": ids["O"], "A": ids["A"], "unknown": "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f", "malformed": "not-a-guid",
        "none": None, "selfDashed": dashed(ids["U"]), "selfCompact": ids["U"], "selfDashedUpper": dashed(ids["U"]).upper(),
        "selfCompactUpper": ids["U"].upper(), "UcompactUpper": ids["U"].upper(), "Udashed": dashed(ids["U"]),
    }[target]


def send(jf, users, st):
    actor = st["actor"]
    token = {"anonymous": None, "badtoken": "0" * 32}.get(actor) if actor in ("anonymous", "badtoken") else users[actor]["token"]
    target = target_id(users, st["target"])
    route = st["route"]
    if route == "path":
        path = f"/Users/{target}/Configuration"
    elif route == "query":
        path = "/Users/Configuration" + (f"?userId={target}" if target else "")
    else:
        path = "/Users/Configuration"
    # "none" means the request names nobody: the target is the caller's own.
    return jf.call("POST", path, token, st["body"], st["contentType"]), path


def delta(config, base):
    """What a Configuration holds that the new user's (base) does not: the fields that differ, and the fields
    that are absent. None stays None (a user whose answer was not a 200)."""
    if config is None:
        return None
    return {"set": {k: v for k, v in config.items() if base.get(k, ABSENT) != v},
            "absent": [k for k in base if k not in config]}


ABSENT = object()


def compact(row, base):
    """Keeps the file small. Each Configuration is written as its difference from the new user's, and the four
    places that must agree (GET /Users/Me, GET /Users/{id} by the administrator and by the user, and the User
    of the next sign-in) as one when they do. A body over 4000 characters is kept as its size, and what Jellyfin
    stored as the length of each list (the Go replay builds such a body itself)."""
    request = row["request"]
    after = row["after"]
    if request and len(request["body"]) > 4000:
        row["request"] = {**request, "body": None, "bodyBytes": len(request["body"])}
        stored = after["me"]["U"]
        row["after"] = {"storedBy": "U", "listLengths": {k: len(v) for k, v in stored.items() if isinstance(v, list)}}
        return
    agree = all(after["byAdmin"][who] == after["me"][who] for who in "UOA") and \
        after["byUserSelf"] == after["me"]["U"] and after["authenticateU"] == after["me"]["U"]
    row["after"] = {"me": {who: delta(after["me"][who], base) for who in "UOA"}, "agree": agree}
    if not agree:
        row["after"]["byAdmin"] = {who: delta(after["byAdmin"][who], base) for who in "UOA"}
        row["after"]["byUserSelf"] = delta(after["byUserSelf"], base)
        row["after"]["authenticateU"] = delta(after["authenticateU"], base)


def config_of(reply):
    return reply["json"]["Configuration"] if reply["status"] == 200 and isinstance(reply["json"], dict) else None


def observe(jf, users):
    after = {"me": {}, "byAdmin": {}}
    for who, user in users.items():
        after["me"][who] = config_of(jf.call("GET", "/Users/Me", user["token"]))
        after["byAdmin"][who] = config_of(jf.call("GET", f"/Users/{user['id']}", users["A"]["token"]))
    after["byUserSelf"] = config_of(jf.call("GET", f"/Users/{users['U']['id']}", users["U"]["token"]))
    return after


def write(path, record):
    """One line for each key and each case, so a diff of two captures shows which case changed."""
    cases = record.pop("cases")
    with open(path, "w") as f:
        f.write("{\n")
        for key, value in record.items():
            f.write(f"{json.dumps(key)}: {json.dumps(value, separators=(',', ':'))},\n")
        f.write('"cases": [\n' + ",\n".join(json.dumps(case, separators=(",", ":")) for case in cases) + "\n]\n}\n")


def main(out_path):
    name = f"jellyscale-capture-jf-{uuid.uuid4().hex[:8]}"
    subprocess.run(["docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8096", IMAGE], check=True, stdout=subprocess.DEVNULL)
    try:
        port = subprocess.run(["docker", "port", name, "8096/tcp"], check=True, capture_output=True, text=True).stdout.split("\n")[0].rsplit(":", 1)[1]
        jf = Jellyfin(f"http://127.0.0.1:{port}")
        wait_ready(jf)
        version = (jf.call("GET", "/System/Info/Public")["json"] or {}).get("Version", "12.4")
        wizard(jf, "capture-admin")
        admin_token, admin_id, _ = sign_in(jf, "capture-admin")

        record = {"about": f"Observed on {IMAGE} (server {version}); see user-configuration-12.4.md", "fields": FIELDS, "cases": []}

        # Cultures.
        with_token = jf.call("GET", "/Localization/Cultures", admin_token)
        without = jf.call("GET", "/Localization/Cultures")
        record["cultures"] = {"status": with_token["status"], "contentType": with_token["contentType"], "entries": with_token["json"],
                              "withoutToken": {"status": without["status"], "text": without["text"]}}

        for number, case in enumerate(cases()):
            users = make_users(jf, admin_token, f"c{number}")
            if number == 0:
                record["newUser"] = {"me": config_of(jf.call("GET", "/Users/Me", users["U"]["token"])),
                                     "rawText": jf.call("GET", "/Users/Me", users["U"]["token"])["text"]}
            for st in case["before"]:
                send(jf, users, st)
            row = {"name": case["name"], "before": case["before"], "request": case["request"]}
            if case["request"]:
                reply, path = send(jf, users, case["request"])
                row["status"], row["contentType"], row["responseText"] = reply["status"], reply["contentType"], reply["text"] if len(reply["text"]) < 4000 else reply["text"][:4000]
            row["after"] = observe(jf, users)
            row["after"]["authenticateU"] = sign_in(jf, f"c{number}-U")[2]["Configuration"]
            compact(row, record["newUser"]["me"])
            record["cases"].append(row)
            print(f"{number:3} {row.get('status', '-')} {case['name']}", flush=True)
        write(out_path, record)
    finally:
        subprocess.run(["docker", "rm", "-fv", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    main(sys.argv[1])
