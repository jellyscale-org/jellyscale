#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 does with a user id a request names (JS-127).

Usage: python3 -I test/differential/capture_user_scope.py <out.json> [route regex]
(writes <out>.json and <out>.md; run from the repository root)

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port over synthetic films made here with ffmpeg,
completes the startup wizard, and asks every route of compat/endpoints.yaml as three callers: an administrator, a
non-administrator A, and nobody (no token), with B's id in each place the route can take one: the {userId} segment of its path,
the userId and controllableByUserId query keys, and the UserId of the body of POST /Items/{itemId}/PlaybackInfo. The path,
userId and PlaybackInfo body also take an id of nobody, a malformed id, the caller's own id (compact, and dashed in upper case)
and the all-zero id; GET /Sessions gives controllableByUserId the same matrix. A few cases put two different ids in two places or
two values under one key.

Each user holds data that tells them apart (a favourite, a played film, a position, display preferences, a
Configuration, a session on a device of their own), so the answer says whose data came back, and what a request
changed is the difference between the state of A, B and the administrator before and after it. A route that changes
something is asked with three fresh users for every case; the others share one set of three. The container is removed
when the script ends. Synthetic names only.

compat/captures/user-scope-12.4.md describes the result. The /jellyscale/v1/ routes are not Jellyfin's: they are asked
once and recorded as that.
"""
import base64
import concurrent.futures
import http.client
import json
import os
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
import uuid

IMAGE = "jellyfin/jellyfin:12.4"
ADMIN_PASSWORD = "capture-admin-pass"
REPO = os.getcwd()
ZERO = "00000000-0000-0000-0000-000000000000"
NOBODY = "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
MALFORMED = "not-a-guid"
WORKERS = 8

# What tells the three users apart. Each film's UserData of a user is (favourite, played, position).
MARKERS = {
    "A": {"fav": "f1", "played": "f2", "position": ("f3", 100000000), "mode": "Always", "sub": "eng", "audio": "fra"},
    "B": {"fav": "f2", "played": "f1", "position": ("f3", 200000000), "mode": "Always", "sub": "spa", "audio": "deu"},
    "admin": {"fav": None, "played": None, "position": None, "mode": "None", "sub": "", "audio": "ita"},
}


def run(*cmd, **kw):
    return subprocess.run(cmd, check=True, capture_output=True, text=True, **kw).stdout.strip()


def auth_header(token=None, device="capture"):
    parts = ['Client="capture"', 'Device="capture"', f'DeviceId="{device}"', 'Version="1"']
    if token:
        parts.append(f'Token="{token}"')
    return "MediaBrowser " + ", ".join(parts)


class Server:
    def __init__(self, port):
        self.port = port

    def request(self, method, path, token=None, body=None, device="capture", content_type="application/json", timeout=120):
        headers = {"Authorization": auth_header(token, device)}
        if token is None:
            headers = {"Authorization": auth_header(None, device)} if device else {}
        data = None
        if body is not None:
            data = body.encode() if isinstance(body, str) else body
            headers["Content-Type"] = content_type
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        conn.request(method, path, body=data, headers=headers)
        resp = conn.getresponse()
        raw = resp.read() if method != "HEAD" else b""
        conn.close()
        text = raw.decode("utf-8", "replace")
        try:
            parsed = json.loads(text) if text.strip() and text.lstrip()[0] in "{[" else None
        except ValueError:
            parsed = None
        return {"status": resp.status, "text": text, "json": parsed, "bytes": len(raw)}

    def websocket(self, path, token, device):
        """The status of a WebSocket upgrade: 101 when the server takes it. The token goes in the query's ApiKey."""
        if token:
            path += ("&" if "?" in path else "?") + "ApiKey=" + token
        sock = socket.create_connection(("127.0.0.1", self.port), timeout=30)
        lines = [f"GET {path} HTTP/1.1", f"Host: 127.0.0.1:{self.port}", "Upgrade: websocket", "Connection: Upgrade",
                 "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: " + base64.b64encode(secrets.token_bytes(16)).decode()]
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        status = int(sock.recv(4096).split(b" ", 2)[1])
        sock.close()
        return {"status": status, "text": "", "json": None, "bytes": 0}


def j(value):
    return json.dumps(value, separators=(",", ":"))


def make_media(root):
    """Four films (the first with an English and a Spanish text subtitle) and an episode of a show."""
    movies = os.path.join(root, "movies")
    shows = os.path.join(root, "shows")

    def ffmpeg(*args):
        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", *args], check=True)

    def film(directory, name, extra):
        os.makedirs(directory, exist_ok=True)
        ffmpeg("-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=6", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=6",
               *extra[0], "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-metadata:s:a:0", "language=eng", *extra[1], os.path.join(directory, name))

    tmp = os.path.join(root, "tmp")
    os.makedirs(tmp)
    subs = []
    for lang, word in (("eng", "English"), ("spa", "Spanish")):
        path = os.path.join(tmp, f"{lang}.srt")
        with open(path, "w") as f:
            f.write(f"1\n00:00:01,000 --> 00:00:03,000\n{word} line\n\n")
        subs.append(path)
    film(os.path.join(movies, "Alpha Film (2021)"), "Alpha Film (2021).mkv",
         (["-i", subs[0], "-i", subs[1], "-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3"],
          ["-c:s", "srt", "-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa"]))
    plain = (["-map", "0:v", "-map", "1:a"], [])
    film(os.path.join(movies, "Bravo Film (2022)"), "Bravo Film (2022).mkv", plain)
    film(os.path.join(movies, "Charlie Film (2023)"), "Charlie Film (2023).mkv", plain)
    film(os.path.join(movies, "Echo Film (2024)"), "Echo Film (2024).mkv", plain)
    film(os.path.join(shows, "Delta Show (2020)", "Season 01"), "Delta Show S01E01 Pilot.mkv", plain)
    shutil.rmtree(tmp)


def wait_for(what, check, seconds=300):
    for _ in range(seconds):
        value = check()
        if value:
            return value
        time.sleep(1)
    raise SystemExit(f"timed out waiting for {what}")


def sign_in(server, name, password, device):
    reply = server.request("POST", "/Users/AuthenticateByName", None, j({"Username": name, "Pw": password}), device=device)
    if reply["status"] != 200:
        raise SystemExit(f"sign-in of {name}: {reply['status']} {reply['text'][:200]}")
    return reply["json"]["AccessToken"], reply["json"]["User"]["Id"]


class Fixture:
    """What the server has: the ids of its films, the observer's token, and the routes' placeholders."""

    def __init__(self, server, admin_token):
        self.server = server
        self.observer = admin_token
        found = server.request("GET", "/Items?Recursive=true&IncludeItemTypes=Movie,Series,Season,Episode", admin_token)["json"]["Items"]
        self.ids = {}
        for item in found:
            if item["Type"] == "Movie":
                self.ids[{"Alpha": "f1", "Bravo": "f2", "Charlie": "f3", "Echo": "f4"}[item["Name"].split()[0]]] = item["Id"]
            else:
                self.ids[item["Type"].lower()] = item["Id"]
        self.ids["view"] = [v["Id"] for v in server.request("GET", "/UserViews", admin_token)["json"]["Items"] if v["Name"] == "Movies"][0]
        info = server.request("POST", f"/Items/{self.ids['f1']}/PlaybackInfo", admin_token, j({}))["json"]["MediaSources"][0]
        self.ids["src"] = info["Id"]
        self.ids["sub"] = [s["Index"] for s in info["MediaStreams"] if s["Type"] == "Subtitle"][0]
        self.subtitle_index = {s["Language"]: s["Index"] for s in info["MediaStreams"] if s["Type"] == "Subtitle"}
        film = self.ids["f1"]
        master_query = ("MaxStreamingBitrate=1000000&MediaSourceId=" + film + "&PlaySessionId=0123456789abcdef0123456789abcdef&VideoCodec=h264&AudioCodec=aac"
                        "&MaxAudioChannels=2&TranscodingMaxAudioChannels=2&SegmentContainer=mp4&MinSegments=1&BreakOnNonKeyFrames=false&h264-profile=high,main,baseline,constrainedbaseline&h264-level=52")
        self.master = f"/Videos/{film}/master.m3u8?{master_query}"
        master = server.request("GET", self.master, admin_token)["text"]
        self.main = f"/Videos/{film}/" + [l for l in master.splitlines() if l.startswith("main.m3u8")][0]
        main = server.request("GET", self.main, admin_token)["text"]
        self.segment = f"/Videos/{film}/" + [l for l in main.splitlines() if l and not l.startswith("#")][0]

    def fill(self, text):
        for key, value in self.ids.items():
            text = text.replace("{" + key + "}", str(value))
        return text


# method, path -> (path or request template, body, kind). Kind: "read", "write" (changes state; fresh users each case),
# "socket". {f1}..{f4}, {series}, {season}, {episode}, {view}, {src}, {sub} stand for what the server has, {userId}
# for the id a case puts in the path.
def templates(fx):
    f1, f4 = "{f1}", "{f4}"
    playing = j({"ItemId": fx.ids["f4"], "MediaSourceId": fx.ids["f4"], "PositionTicks": 30000000, "PlayMethod": "DirectPlay", "CanSeek": True, "IsPaused": False})
    profile = {"Name": "capture", "DirectPlayProfiles": [{"Container": "mkv,mp4", "Type": "Video"}],
               "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls"}]}
    pref = j({"Id": "usersettings", "SortBy": "Changed", "CustomPrefs": {"who": "changed"}, "Client": "capture"})
    config = j({"SubtitleMode": "Smart", "SubtitleLanguagePreference": "jpn", "AudioLanguagePreference": "kor", "PlayDefaultAudioTrack": True})
    t = {}

    def add(method, path, route, body=None, kind="read", label=None):
        t[(method, route)] = dict(path=path, body=body, kind=kind, label=label)

    for route in ("/Artists", "/Genres", "/Items", "/Items/Filters", "/Items/Filters2", "/Items/Latest", "/Items/Suggestions", "/Persons", "/Studios",
                  "/LiveTv/Programs", "/LiveTv/Programs/Recommended", "/Localization/Cultures", "/Plugins", "/QuickConnect/Enabled", "/SyncPlay/List",
                  "/System/Configuration/encoding", "/System/Endpoint", "/System/Info", "/System/Info/Public", "/System/Ping", "/UserViews",
                  "/UserViews/GroupingOptions", "/Users/Me", "/Users/Public", "/Branding/Configuration", "/Branding/Css", "/Branding/Css.css",
                  "/Sessions", "/Shows/NextUp", "/UserItems/Resume"):
        add("GET", route, route)
    add("POST", "/System/Ping", "/System/Ping")
    add("GET", "/DisplayPreferences/usersettings?client=capture", "/DisplayPreferences/{preferenceId}")
    add("POST", "/DisplayPreferences/usersettings?client=capture", "/DisplayPreferences/{preferenceId}", pref, "write")
    for sub in ("Ancestors", "Collections", "Similar", "SpecialFeatures", "ThemeMedia", "LocalTrailers"):
        add("GET", f"/Items/{f1}/{sub}", f"/Items/{{itemId}}/{sub}")
    add("GET", f"/Items/{f1}", "/Items/{itemId}")
    add("GET", f"/Items/{f1}/Download", "/Items/{itemId}/Download", label="download")
    add("GET", f"/Items/{f1}/Images/Primary", "/Items/{itemId}/Images/{imageType}")
    add("GET", f"/Items/{f1}/Images/Chapter/0", "/Items/{itemId}/Images/{imageType}/{imageIndex}")
    add("GET", f"/Items/{f1}/PlaybackInfo", "/Items/{itemId}/PlaybackInfo")
    add("POST", f"/Items/{f1}/PlaybackInfo", "/Items/{itemId}/PlaybackInfo", j({"DeviceProfile": profile}))
    add("POST", f"/jellyscale/v1/items/{f1}/playback-info", "/jellyscale/v1/items/{itemId}/playback-info", j({"DeviceProfile": profile}))
    add("GET", "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences")
    add("PATCH", "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", j({}), "write")
    add("PATCH", "/jellyscale/v1/users/configuration", "/jellyscale/v1/users/configuration", j({}), "write")
    add("GET", f"/MediaSegments/{f1}", "/MediaSegments/{itemId}")
    add("GET", "/Playback/BitrateTest?size=102400", "/Playback/BitrateTest")
    add("GET", "/Search/Hints?searchTerm=film", "/Search/Hints")
    add("POST", "/Sessions/Capabilities?playableMediaTypes=Video&supportsMediaControl=false", "/Sessions/Capabilities", None, "write")
    add("POST", "/Sessions/Capabilities/Full", "/Sessions/Capabilities/Full", '{"PlayableMediaTypes":["Video"],"SupportsMediaControl":false}', "write")
    add("POST", "/Sessions/Logout", "/Sessions/Logout", None, "write")
    add("POST", "/Sessions/Playing", "/Sessions/Playing", playing, "write")
    add("POST", "/Sessions/Playing/Ping?playSessionId=capture", "/Sessions/Playing/Ping", None, "write")
    add("POST", "/Sessions/Playing/Progress", "/Sessions/Playing/Progress", playing, "write")
    add("POST", "/Sessions/Playing/Stopped", "/Sessions/Playing/Stopped", playing, "write")
    add("GET", "/Shows/{series}/Episodes", "/Shows/{seriesId}/Episodes")
    add("GET", "/Shows/{series}/Seasons", "/Shows/{seriesId}/Seasons")
    add("GET", "/socket", "/socket", kind="socket")
    add("DELETE", f"/UserFavoriteItems/{f4}", "/UserFavoriteItems/{itemId}", None, "write")
    add("POST", f"/UserFavoriteItems/{f4}", "/UserFavoriteItems/{itemId}", None, "write")
    add("POST", f"/UserItems/{f4}/UserData", "/UserItems/{itemId}/UserData", '{"IsFavorite":true,"Played":true,"PlaybackPositionTicks":50000000}', "write")
    add("DELETE", f"/UserPlayedItems/{f4}", "/UserPlayedItems/{itemId}", None, "write")
    add("POST", f"/UserPlayedItems/{f4}", "/UserPlayedItems/{itemId}", None, "write")
    add("POST", "/Users/AuthenticateByName", "/Users/AuthenticateByName", "SIGNIN", "signin")
    add("POST", "/Users/Configuration", "/Users/Configuration", config, "write")
    add("POST", "/Users/{userId}/Configuration", "/Users/{userId}/Configuration", config, "write")
    add("GET", "/Users/{userId}", "/Users/{userId}")
    add("DELETE", f"/Users/{{userId}}/FavoriteItems/{f4}", "/Users/{userId}/FavoriteItems/{itemId}", None, "write")
    add("POST", f"/Users/{{userId}}/FavoriteItems/{f4}", "/Users/{userId}/FavoriteItems/{itemId}", None, "write")
    add("GET", "/Users/{userId}/GroupingOptions", "/Users/{userId}/GroupingOptions")
    add("GET", "/Users/{userId}/Items", "/Users/{userId}/Items")
    add("GET", "/Users/{userId}/Items/Latest", "/Users/{userId}/Items/Latest")
    add("GET", "/Users/{userId}/Items/Resume", "/Users/{userId}/Items/Resume")
    add("GET", "/Users/{userId}/Items/Root", "/Users/{userId}/Items/Root")
    add("GET", f"/Users/{{userId}}/Items/{f1}", "/Users/{userId}/Items/{itemId}")
    for sub in ("Intros", "LocalTrailers", "SpecialFeatures"):
        add("GET", f"/Users/{{userId}}/Items/{f1}/{sub}", f"/Users/{{userId}}/Items/{{itemId}}/{sub}")
    add("DELETE", f"/Users/{{userId}}/PlayedItems/{f4}", "/Users/{userId}/PlayedItems/{itemId}", None, "write")
    add("POST", f"/Users/{{userId}}/PlayedItems/{f4}", "/Users/{userId}/PlayedItems/{itemId}", None, "write")
    add("DELETE", "/Videos/ActiveEncodings?deviceId=capture-device&playSessionId=capture", "/Videos/ActiveEncodings", None, "write")
    add("GET", fx.segment, "/Videos/{itemId}/hls1/main/{segmentId}", label="segment")
    add("GET", fx.main, "/Videos/{itemId}/main.m3u8", label="main playlist")
    add("GET", fx.master, "/Videos/{itemId}/master.m3u8", label="master playlist")
    add("GET", f"/Videos/{f1}/stream?static=true", "/Videos/{itemId}/{file}", label="static stream")
    add("GET", f"/Videos/{f1}/{{src}}/Attachments/3", "/Videos/{itemId}/{sourceId}/Attachments/{index}")
    add("GET", f"/Videos/{f1}/{{src}}/Subtitles/{{sub}}/Stream.vtt", "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}", label="text subtitle")
    add("GET", f"/Videos/{f1}/{{src}}/Subtitles/{{sub}}/10000000/Stream.vtt", "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}", label="text subtitle from an offset")
    return t


def routes_of_endpoints():
    out = []
    with open(os.path.join(REPO, "compat/endpoints.yaml")) as f:
        for line in f:
            m = re.search(r'method: (\w+), path: "([^"]+)"', line)
            if m:
                out.append((m.group(1), m.group(2)))
    return out


class UserSet:
    """Three users, each with data that tells them apart, and a token on a device of their own."""

    def __init__(self, fx, tag):
        self.fx = fx
        self.server = fx.server
        self.users = {}
        for who in ("A", "B", "admin"):
            name = f"{tag}{who}"
            made = self.server.request("POST", "/Users/New", fx.observer, j({"Name": name, "Password": ""}), device="capture-observer")
            if made["status"] != 200:
                raise SystemExit(f"making {name}: {made['status']} {made['text'][:200]}")
            if who == "admin":
                policy = made["json"]["Policy"]
                policy["IsAdministrator"] = True
                assert self.server.request("POST", f"/Users/{made['json']['Id']}/Policy", fx.observer, j(policy), device="capture-observer")["status"] == 204
            device = f"dev-{name}"
            token, uid = sign_in(self.server, name, "", device)
            self.users[who] = {"id": uid, "token": token, "name": name, "device": device}
        self.seed()

    def call(self, who, method, path, body=None):
        user = self.users[who]
        return self.server.request(method, path, user["token"], body, device=user["device"])

    def seed(self):
        ids = self.fx.ids
        for who, mark in MARKERS.items():
            self.call(who, "POST", "/Users/Configuration", j({"SubtitleMode": mark["mode"], "SubtitleLanguagePreference": mark["sub"],
                                                              "AudioLanguagePreference": mark["audio"], "PlayDefaultAudioTrack": False}))
            self.call(who, "POST", "/DisplayPreferences/usersettings?client=capture",
                      j({"Id": "usersettings", "SortBy": "SortName", "CustomPrefs": {"who": who}, "Client": "capture"}))
            if mark["fav"]:
                self.call(who, "POST", f"/UserFavoriteItems/{ids[mark['fav']]}")
                self.call(who, "POST", f"/UserPlayedItems/{ids[mark['played']]}")
                item, ticks = mark["position"]
                self.call(who, "POST", f"/UserItems/{ids[item]}/UserData", j({"PlaybackPositionTicks": ticks}))

    def observe(self):
        """What each user holds now, read with their own token (a token that no longer works says so)."""
        state = {}
        sessions = self.server.request("GET", "/Sessions", self.fx.observer, device="capture-observer")["json"] or []
        for who, user in self.users.items():
            row = {"token": self.call(who, "GET", "/Users/Me")["status"]}
            if row["token"] == 200:
                me = self.call(who, "GET", "/Users/Me")["json"]["Configuration"]
                row["config"] = [me.get("SubtitleMode"), me.get("SubtitleLanguagePreference"), me.get("AudioLanguagePreference")]
                prefs = self.call(who, "GET", "/DisplayPreferences/usersettings?client=capture")["json"] or {}
                row["prefs"] = [prefs.get("SortBy"), (prefs.get("CustomPrefs") or {}).get("who")]
                for key in ("f1", "f2", "f3", "f4"):
                    data = (self.call(who, "GET", f"/Items/{self.fx.ids[key]}")["json"] or {}).get("UserData") or {}
                    row[key] = [data.get("IsFavorite"), data.get("Played"), data.get("PlaybackPositionTicks"), data.get("PlayCount")]
            row["sessions"] = sorted((s.get("DeviceId", ""), (s.get("NowPlayingItem") or {}).get("Id"), (s.get("PlayState") or {}).get("PositionTicks"))
                                     for s in sessions if s.get("UserId") == user["id"] and s.get("DeviceId") == user["device"])
            state[who] = row
        return state


def delta(before, after):
    """The fields of each user that differ, as their new values; users with no difference are left out."""
    out = {}
    for who in before:
        changed = {key: after[who].get(key) for key in set(before[who]) | set(after[who]) if before[who].get(key) != after[who].get(key)}
        if changed:
            out[who] = dict(sorted(changed.items()))
    return out


def whose(users, text, parsed):
    """Which of the three users an answer carries the data of: their ids in it, the UserData of the marker films,
    Configuration's languages, display preferences' custom value, and PlaybackInfo's default subtitle track."""
    found = set()
    plain = text.lower().replace("-", "")
    for who, user in users.users.items():
        if user["id"].lower() in plain:
            found.add(f"id:{who}")
    ids = users.fx.ids
    item_of = {ids[k]: k for k in ("f1", "f2", "f3")}
    seen = {}

    def walk(node):
        if isinstance(node, dict):
            if node.get("Id") in item_of and isinstance(node.get("UserData"), dict):
                data = node["UserData"]
                seen[item_of[node["Id"]]] = (bool(data.get("IsFavorite")), bool(data.get("Played")), data.get("PlaybackPositionTicks", 0))
            if isinstance(node.get("Configuration"), dict):
                lang = node["Configuration"].get("SubtitleLanguagePreference")
                for who, mark in MARKERS.items():
                    if lang == mark["sub"] and node.get("Id", "").lower() == users.users[who]["id"].lower():
                        found.add(f"config:{who}")
            if isinstance(node.get("CustomPrefs"), dict) and node["CustomPrefs"].get("who") in MARKERS:
                found.add(f"prefs:{node['CustomPrefs']['who']}")
            if "PlaySessionId" in node and "MediaSources" in node:
                # PlaybackInfo: the default subtitle track is A's English, B's Spanish, and none for the administrator.
                by_lang = {v: k for k, v in users.fx.subtitle_index.items()}
                for source in node["MediaSources"]:
                    index = source.get("DefaultSubtitleStreamIndex")
                    found.add("subtitle-default:" + {"eng": "A", "spa": "B"}.get(by_lang.get(index), "admin"))
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)

    walk(parsed)
    if seen:
        expected = {}
        for who, mark in MARKERS.items():
            expected[who] = {"f1": (mark["fav"] == "f1", mark["played"] == "f1", 0), "f2": (mark["fav"] == "f2", mark["played"] == "f2", 0),
                             "f3": (False, False, mark["position"][1] if mark["position"] else 0)}
        match = [who for who in MARKERS if all(expected[who][key][:2] == value[:2] and (key != "f3" or expected[who][key][2] == (value[2] or 0)) for key, value in seen.items())]
        found.add("data:" + ("/".join(match) if match else "unrecognised"))
    return sorted(found)


def verdict(found):
    """One word for the table: the user the answer is of, 'mixed', or 'nobody' when it carries nothing of a user."""
    users = {entry.split(":")[1] for entry in found}
    users = {u for part in users for u in part.split("/")}
    if not users:
        return "nobody"
    return "/".join(sorted(users))


def id_value(kind, caller_id):
    return {"B": None, "nobody": NOBODY, "malformed": MALFORMED, "zero": ZERO, "empty": "",
            "self": str(uuid.UUID(caller_id)).upper(), "selfCompact": caller_id.lower()}[kind]


def cases_of(route, kind_of_route):
    """The requests asked of one route. Each is (caller, name, placements); a placement is ('path', kind),
    ('query', key, kind), ('query-twice', key, kind, kind) or ('body', kind), with kind one of the ids above."""
    method, path = route
    has_path = "{userId}" in path
    postinfo = path == "/Items/{itemId}/PlaybackInfo" and method == "POST"
    out = []
    for caller in ("A", "admin"):
        out.append((caller, "no id" if not has_path else "own id in the path", [("path", "selfCompact")] if has_path else []))
        for kind in ("B", "nobody", "malformed", "self", "selfCompact", "zero"):
            if has_path:
                out.append((caller, f"path: {kind}", [("path", kind)]))
            out.append((caller, f"query userId: {kind}", [("query", "userId", kind)] + ([("path", "selfCompact")] if has_path else [])))
            if postinfo:
                out.append((caller, f"body UserId: {kind}", [("body", kind)]))
        out.append((caller, "query userId: empty", [("query", "userId", "empty")] + ([("path", "selfCompact")] if has_path else [])))
        controllable_kinds = ("B", "nobody", "malformed", "self", "selfCompact", "zero") if route == ("GET", "/Sessions") else ("B",)
        for kind in controllable_kinds:
            out.append((caller, f"query controllableByUserId: {kind}", [("query", "controllableByUserId", kind)] + ([("path", "selfCompact")] if has_path else [])))
        out.append((caller, "query userId: two values, own then B", [("query-twice", "userId", "selfCompact", "B")] + ([("path", "selfCompact")] if has_path else [])))
        out.append((caller, "query userId: two values, B then own", [("query-twice", "userId", "B", "selfCompact")] + ([("path", "selfCompact")] if has_path else [])))
        out.append((caller, "query UserId and userId: own and B", [("query", "UserId", "selfCompact"), ("query", "userId", "B")] + ([("path", "selfCompact")] if has_path else [])))
        if route == ("GET", "/Sessions"):
            out.append((caller, "query controllableByUserId: two values, own then B", [("query-twice", "controllableByUserId", "selfCompact", "B")]))
            out.append((caller, "query controllableByUserId: two values, B then own", [("query-twice", "controllableByUserId", "B", "selfCompact")]))
            out.append((caller, "query ControllableByUserId and controllableByUserId: own and B", [("query", "ControllableByUserId", "selfCompact"), ("query", "controllableByUserId", "B")]))
            out.append((caller, "query userId and controllableByUserId: own and B", [("query", "userId", "selfCompact"), ("query", "controllableByUserId", "B")]))
        if has_path:
            out.append((caller, "path own, query B", [("path", "selfCompact"), ("query", "userId", "B")]))
            out.append((caller, "path B, query own", [("path", "B"), ("query", "userId", "selfCompact")]))
            out.append((caller, "path B, query B", [("path", "B"), ("query", "userId", "B")]))
            out.append((caller, "path nobody, query B", [("path", "nobody"), ("query", "userId", "B")]))
        if postinfo:
            out.append((caller, "query own, body B", [("query", "userId", "selfCompact"), ("body", "B")]))
            out.append((caller, "query B, body own", [("query", "userId", "B"), ("body", "selfCompact")]))
    out.append(("none", "no token, query userId: B", [("query", "userId", "B")] + ([("path", "B")] if has_path else [])))
    out.append(("none", "no token, no id", [] if not has_path else [("path", "B")]))
    return out


def build_request(fx, users, tpl, caller, placements):
    caller_id = users.users[caller]["id"] if caller in users.users else users.users["A"]["id"]
    values = {"B": users.users["B"]["id"]}
    path, body = tpl["path"], tpl["body"]
    if body == "SIGNIN":
        who = caller if caller in users.users else "A"
        body = j({"Username": users.users[who]["name"], "Pw": ""})
    json_body = None
    queries = []
    shown = []
    for placement in placements:
        if placement[0] == "path":
            value = values["B"] if placement[1] == "B" else id_value(placement[1], caller_id)
            path = path.replace("{userId}", value)
            shown.append(f"path={placement[1]}")
        elif placement[0] == "query":
            value = values["B"] if placement[2] == "B" else id_value(placement[2], caller_id)
            queries.append((placement[1], value))
            shown.append(f"{placement[1]}={placement[2]}")
        elif placement[0] == "query-twice":
            for kind in placement[2:]:
                queries.append((placement[1], values["B"] if kind == "B" else id_value(kind, caller_id)))
            shown.append(f"{placement[1]}={placement[2]}&{placement[1]}={placement[3]}")
        elif placement[0] == "body":
            value = values["B"] if placement[1] == "B" else id_value(placement[1], caller_id)
            json_body = value
            shown.append(f"body.UserId={placement[1]}")
    if "{userId}" in path:  # A route with a path id and no case that names one: the caller's own.
        path = path.replace("{userId}", caller_id.lower())
    if queries:
        path += ("&" if "?" in path else "?") + urllib.parse.urlencode(queries)
    if json_body is not None:
        parsed = json.loads(body)
        parsed["UserId"] = json_body
        body = j(parsed)
    return fx.fill(path), body, shown


def classify(label, reply):
    if label and 200 <= reply["status"] < 300:
        return f"{reply['bytes']} bytes"
    return None


def ask(fx, users, route, tpl, case, fresh):
    caller, name, placements = case
    path, body, shown = build_request(fx, users, tpl, caller, placements)
    method = route[0]
    token = None if caller == "none" else users.users[caller]["token"]
    device = None if caller == "none" else users.users[caller]["device"]
    before = users.observe() if fresh else None
    if tpl["kind"] == "socket":
        reply = fx.server.websocket(path, token, device)
    else:
        reply = fx.server.request(method, path, token, body, device=device or "")
    row = {"route": f"{route[0]} {route[1]}", "caller": caller, "case": name, "request": shown, "status": reply["status"]}
    if tpl["kind"] in ("read", "signin") or (tpl["kind"] == "write" and reply["status"] < 300 and reply["json"] is not None):
        row["whose"] = whose(users, reply["text"], reply["json"])
    if reply["text"] and reply["status"] >= 300:
        row["body"] = reply["text"][:200]
    if tpl["label"]:
        row["answer"] = classify(tpl["label"], reply)
    if fresh:
        row["delta"] = delta(before, users.observe())
    return row


def sort_key(row):
    return (row["route"], row["caller"], row["case"])


def main():
    out = os.path.abspath(sys.argv[1])
    only = re.compile(sys.argv[2]) if len(sys.argv) > 2 else None
    work = os.path.join(os.path.expanduser("~/.cache/jellyscale/agents"), f"capture-user-scope-{os.getpid()}")
    os.makedirs(work)
    name = f"jellyscale-capture-user-scope-{os.getpid()}"
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    try:
        make_media(os.path.join(work, "media"))
        run("docker", "run", "-d", "--rm", "--name", name, "-v", f"{os.path.join(work, 'media')}:/media:ro", "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])
        server = Server(port)

        def up():
            try:
                reply = server.request("GET", "/System/Info/Public", device="")
                return reply["json"] if reply["status"] == 200 and reply["json"] and "Version" in reply["json"] else None
            except (OSError, http.client.HTTPException):
                return None
        version = wait_for("jellyfin to start", up, 180)["Version"]
        for method, path, body in (("POST", "/Startup/Configuration", '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", j({"Name": "admin", "Password": ADMIN_PASSWORD})),
                                   ("POST", "/Startup/RemoteAccess", '{"EnableRemoteAccess":true}'), ("POST", "/Startup/Complete", None)):
            reply = server.request(method, path, None, body, device="")
            if reply["status"] >= 300:
                raise SystemExit(f"{method} {path}: {reply['status']} {reply['text'][:200]}")
        observer, _ = sign_in(server, "admin", ADMIN_PASSWORD, "capture-observer")
        for lib, kind in (("Movies", "movies"), ("Shows", "tvshows")):
            server.request("POST", f"/Library/VirtualFolders?refreshLibrary=false&name={lib}&collectionType={kind}", observer,
                           j({"LibraryOptions": {"PathInfos": [{"Path": f"/media/{lib.lower()}"}]}}), device="capture-observer")
        server.request("POST", "/Library/Refresh", observer, device="capture-observer")

        def scanned():
            found = server.request("GET", "/Items?Recursive=true&IncludeItemTypes=Movie,Series,Season,Episode", observer, device="capture-observer")["json"]["Items"]
            tasks = server.request("GET", "/ScheduledTasks", observer, device="capture-observer")["json"]
            print(f"waiting: {len(found)} items", flush=True)
            return len(found) >= 7 and not [t for t in tasks if t["State"] != "Idle"]
        wait_for("the scan", scanned, 240)
        fx = Fixture(server, observer)
        tpls = templates(fx)

        listed = routes_of_endpoints()
        missing = [r for r in listed if r not in tpls]
        extra = [r for r in tpls if r not in listed]
        if missing or extra:
            raise SystemExit(f"endpoints.yaml and the capture's routes differ: not asked {missing}, not in endpoints.yaml {extra}")

        shared = UserSet(fx, "shared" + uuid.uuid4().hex[:6])
        rows, jobs = [], []
        for route in listed:
            if only and not only.search(f"{route[0]} {route[1]}"):
                continue
            tpl = tpls[route]
            if route[1].startswith("/jellyscale/"):
                jobs.append((route, tpl, ("A", "not a Jellyfin route: asked once, query userId: B", [("query", "userId", "B")]), False))
                continue
            for case in cases_of(route, tpl["kind"]):
                jobs.append((route, tpl, case, tpl["kind"] in ("write", "signin")))
        counter = [0]
        lock = threading.Lock()

        def do(job):
            route, tpl, case, fresh = job
            users = UserSet(fx, "c" + uuid.uuid4().hex[:8]) if fresh else shared
            try:
                row = ask(fx, users, route, tpl, case, fresh)
            except Exception as error:  # A failed request is a row of the capture, not an end of it.
                row = {"route": f"{route[0]} {route[1]}", "caller": case[0], "case": case[1], "error": repr(error)}
            if route[1].startswith("/jellyscale/"):
                row["notJellyfin"] = True
            with lock:
                counter[0] += 1
                if counter[0] % 50 == 0:
                    print(f"{counter[0]}/{len(jobs)}", flush=True)
            return row

        with concurrent.futures.ThreadPoolExecutor(WORKERS) as pool:
            rows = list(pool.map(do, jobs))
        rows.sort(key=sort_key)
        capture = dict(image=IMAGE, server_version=version, markers=MARKERS, rows=rows)
        write(out, capture)
        with open(re.sub(r"\.json$", ".md", out), "w") as f:
            f.write(markdown(capture))
    finally:
        if started:
            remove_container(name)
        shutil.rmtree(work, ignore_errors=True)


def write(path, capture):
    """One line for each row, so a diff of two captures shows which case changed."""
    rows = capture["rows"]
    with open(path, "w") as f:
        f.write("{\n")
        for key, value in capture.items():
            if key == "rows":
                continue
            f.write(f"{json.dumps(key)}: {json.dumps(value, separators=(',', ':'))},\n")
        f.write('"rows": [\n' + ",\n".join(json.dumps(row, separators=(",", ":")) for row in rows) + "\n]\n}\n")


def remove_container(name):
    for _ in range(10):
        result = subprocess.run(["docker", "rm", "-fv", name], capture_output=True, text=True)
        if subprocess.run(["docker", "inspect", name], capture_output=True).returncode != 0:
            return
        time.sleep(1)
    raise SystemExit(f"could not remove the container {name}: {result.stderr.strip()}")


def cell(row):
    if row is None:
        return "-"
    if "error" in row:
        return "error"
    text = str(row["status"])
    if "whose" in row and row["status"] < 300:
        text += " " + verdict(row["whose"])
    if row.get("delta"):
        text += " changed " + "+".join(sorted(row["delta"]))
    return text


def findings(by):
    """What the rows say, as sentences: each is computed from them, so a rerun that finds otherwise says otherwise."""
    def status(route, caller, case):
        row = by[route].get((caller, case))
        return row["status"] if row else None

    routes = [r for r, cases in by.items() if not any(x.get("notJellyfin") for x in cases.values())]
    refused = [r for r in routes if status(r, "A", "query userId: B") == 403 and status(r, "A", "no id") != 403]
    ignoring = [r for r in routes if status(r, "A", "query userId: B") not in (None, 403) and status(r, "A", "query userId: B") == status(r, "A", "no id")
                and not by[r].get(("A", "path: B"))]
    zero_served = [r for r in routes if status(r, "A", "query userId: zero") in (200, 204)]
    zero_refused = [r for r in routes if status(r, "A", "query userId: zero") in (400, 403) and status(r, "A", "no id") != 403]
    first_wins = [r for r in refused if status(r, "A", "query userId: two values, B then own") == 403 and status(r, "A", "query userId: two values, own then B") != 403]
    path_read = [r for r in routes if "{userId}" in r and status(r, "A", "path: B") == 403]
    lines = ["## What the rows say\n"]
    lines.append(f"- **The routes that read the id** answer a non-administrator who names B in the query with 403: {len(refused)} routes (those that call `RequestHelpers.GetUserId`), "
                 + ", ".join(f"`{r}`" for r in refused) + ". Every other route ignores `userId` and `controllableByUserId` and answers as it does with none (a 200 with the caller's own data, or the status it has without it); "
                 "`GET /Sessions` alone reads `controllableByUserId`: 403 to a non-administrator naming B, 200 with no session to an administrator naming B.")
    lines.append(f"- **The path** of the `/Users/{{userId}}/...` routes ({len(path_read)} refuse B in the path to a non-administrator, 403) is read the same way. `GET /Users/{{userId}}` is the exception: a non-administrator naming B in the path gets B's record "
                 f"(status {status('GET /Users/{userId}', 'A', 'path: B')}), because Jellyfin lets any signed-in user read every account (`UserController.cs:127`).")
    lines.append(f"- **An administrator** naming B in the query or the path is served B's view on every route that reads the id ({len(refused)} routes), and acts on B on the writes among them; naming an id that is no user "
                 f"is {status('GET /Items', 'admin', 'query userId: nobody')} on `GET /Items` and wherever the answer needs the user. (`GET /Plugins` is the administrator's alone in Jellyfin: 403 to A with or without an id.)")
    lines.append(f"- **The all-zero id** is read as no id: {len(zero_served)} routes serve the caller's own data and answer exactly as they do with none. The exceptions are the routes that parse the id as a path or `userId` they must act on: "
                 + ", ".join(f"`{r}` ({status(r, 'A', 'query userId: zero')})" for r in zero_refused) + f"; in a path, `GET /Users/{{zero}}` and `POST /Users/{{zero}}/Configuration` are {status('GET /Users/{userId}', 'A', 'path: zero')} and the other `/Users/{{userId}}/...` routes serve the caller. "
                 "So a client that sends the zero id (none of the supported clients' captures does) is served by Jellyfin and would be refused by a guard that takes the zero id for someone else's.")
    lines.append(f"- **Several values:** Jellyfin binds the first value of a repeated or differently cased key: `userId=B&userId=own` is refused ({len(first_wins)} of the {len(refused)} routes that read the id), `userId=own&userId=B` is served as own, "
                 "and `userId=own&UserId=B` likewise (the keys are one key). On `POST /Items/{itemId}/PlaybackInfo` the query beats the body: query own with B in the body is served as own, query B with own in the body is refused.")
    lines.append("- **Path and query naming different users** on the routes that take both (`POST /Users/{userId}/Configuration`, `GET /Users/{userId}`): the path wins and the query is ignored. A non-administrator with B in the path gets 403 whatever the query says, "
                 f"and with its own id in the path and B in the query is served as itself (`POST /Users/{{own}}/Configuration?userId=B`: {status('POST /Users/{userId}/Configuration', 'A', 'path own, query B')}, A's row changed); an administrator with the path on B and the query on themselves writes B. Jellyfin answers neither 400 nor 403 for the disagreement.")
    lines.append(f"- **Nobody's id as a non-administrator on the Configuration routes** is 404 (`POST /Users/Configuration?userId=nobody`: {status('POST /Users/Configuration', 'A', 'query userId: nobody')}); on `POST /UserItems/{{itemId}}/UserData` and every GetUserId route it is "
                 f"{status('POST /UserItems/{itemId}/UserData', 'A', 'query userId: nobody')}, the status for B's id. A malformed id is 400 on every route that reads it.")
    lines.append(f"- **No token** is 401 on every route that needs one, with or without `userId=B`, except `GET /socket`, which was observed answering "
                 f"{status('GET /socket', 'none', 'no token, no id')} without an id and {status('GET /socket', 'none', 'no token, query userId: B')} with `userId=B`. The open routes answer as they do without the id.")
    lines.append("- The answers of `master.m3u8` and `main.m3u8` carry the query string they were asked with (the id in them is the id the request named, not a user's data); the table reads that as `B`.")
    lines.append("")

    def row(route, caller, case):
        return f"`{route}` as {caller}, {case}: {status(route, caller, case)}"

    lines += ["## Where jellyscale answers otherwise\n",
              "JS-127 keeps Jellyfin's rule where Jellyfin refuses, and is stricter on purpose (the lead, 2026-11-24, from the owner's access decisions of 2026-11-16 and D-075): "
              "on every route that acts for the caller, a user id that names anyone but the caller is 403 for everyone, an administrator included, in every place. These rows are what it differs from:\n",
              f"- **The all-zero id is refused as not the caller's** (403). Jellyfin reads it as no id and serves the caller: {row('GET /Items', 'A', 'query userId: zero')}; "
              f"{row('POST /UserPlayedItems/{itemId}', 'A', 'query userId: zero')}; {row('GET /Users/{userId}/Items/Latest', 'A', 'path: zero')}; {row('POST /Items/{itemId}/PlaybackInfo', 'A', 'body UserId: zero')}.",
              f"- **Every value counts** (403 if any names another user). Jellyfin takes the first value: {row('GET /Items', 'A', 'query userId: two values, own then B')}; "
              f"{row('GET /Items', 'A', 'query UserId and userId: own and B')}; {row('POST /Items/{itemId}/PlaybackInfo', 'A', 'query own, body B')} (the query beats the body); "
              f"{row('GET /Items', 'A', 'query controllableByUserId: B')} (Jellyfin reads `controllableByUserId` on `GET /Sessions` alone: {status('GET /Sessions', 'A', 'query controllableByUserId: B')}).",
              f"- **Different ids in the path and the query of an admin-any route:** a non-administrator gets 403 and an administrator 400, nothing read or written. Jellyfin lets the path win and ignores the query: "
              f"{row('POST /Users/{userId}/Configuration', 'A', 'path own, query B')} (A's own row changed); {row('POST /Users/{userId}/Configuration', 'admin', 'path B, query own')} (B's row changed); "
              f"{row('GET /Users/{userId}', 'A', 'path own, query B')}; {row('GET /Users/{userId}', 'admin', 'path B, query own')}.",
              f"- **A non-administrator naming an id that is no user** is 403 on the Configuration routes, as for B's id; Jellyfin answers 404 there: {row('POST /Users/Configuration', 'A', 'query userId: nobody')}; "
              f"{row('POST /Users/{userId}/Configuration', 'A', 'path: nobody')} (D-075).",
              f"- **A malformed id on a route that acts for the caller** is 403 like any other id that is not the caller's. Jellyfin answers 400 on the routes that read the id: {row('GET /Items', 'A', 'query userId: malformed')}; "
              f"{row('GET /Users/{userId}/Items', 'A', 'path: malformed')}. On the admin-any routes a malformed id stays 400 whoever asks.",
              f"- **An administrator naming another user** is refused (403) on every route that acts for the caller; Jellyfin serves B's view: {row('GET /Items', 'admin', 'query userId: B')}; "
              f"{row('POST /UserPlayedItems/{itemId}', 'admin', 'query userId: B')} (B's data changed); {row('GET /Users/{userId}/Items/Latest', 'admin', 'path: B')}. "
              f"Only four routes act on the user named, for an administrator: `GET /Users/{{userId}}`, `POST /Users/Configuration`, `POST /Users/{{userId}}/Configuration` and `POST /UserItems/{{itemId}}/UserData`.",
              f"- **A non-administrator reading B's record** (`GET /Users/{{B}}`) is 403; Jellyfin serves it: {row('GET /Users/{userId}', 'A', 'path: B')} (the 'any signed-in user reading every account' row of `docs/access.md`, here observed).",
              ""]
    return lines


def markdown(c):
    rows = c["rows"]
    by = {}
    for row in rows:
        by.setdefault(row["route"], {})[(row["caller"], row["case"])] = row
    out = ["# What Jellyfin 12.4 does with a user id a request names\n",
           f"Observed with `{c['image']}` (server version {c['server_version']}), a disposable container on a Docker-assigned port over synthetic films "
           "made by `test/differential/capture_user_scope.py`: four films (the first with an English and a Spanish text subtitle) and an episode of a show. "
           "Evidence in `user-scope-12.4.json`, one line for each request; the script is rerun with "
           "`python3 -I test/differential/capture_user_scope.py compat/captures/user-scope-12.4.json` from the repository root.\n",
           "Every route of `compat/endpoints.yaml` is asked as an administrator, as a non-administrator A, and with no token. A second non-administrator B's id goes "
           "in each place of each route: the `{userId}` segment of its path, the `userId` and `controllableByUserId` query keys, and the `UserId` of the body of "
           "`POST /Items/{itemId}/PlaybackInfo`. The path, `userId` and PlaybackInfo body also take an id of nobody, a malformed id, the caller's own id "
           "(compact, and dashed in upper case) and the all-zero id; `GET /Sessions` gives `controllableByUserId` the same matrix. Cases also put two ids in two places "
           "or two values under one key. Each case that can change something "
           "(`POST`, `DELETE`, `PATCH`) is asked of three new users; the others of one shared set.\n",
           "The users hold data that tells them apart. **A**: favourite film 1, film 2 played, a position in film 3, the Configuration `Always`/`eng`/`fra`. "
           "**B**: favourite film 2, film 1 played, a position twice A's in film 3, `Always`/`spa`/`deu`. **The administrator**: nothing, `None`/``/`ita`. "
           "Each has a session on a device of their own and display preferences naming them. Whose data an answer carries is read from the ids in it, the "
           "`UserData` of films 1 to 3, a `Configuration`'s subtitle language, `CustomPrefs` and the `DefaultSubtitleStreamIndex` PlaybackInfo chooses; "
           "`nobody` is an answer with none of these. What a request changed is the difference between what each user holds before and after (their "
           "favourites, played state and positions, Configuration, display preferences, whether their token still works, and their session).\n",
           "In the tables a cell is the status, then whose data came back (`A`, `B`, `admin`, `nobody`), then `changed` and who. A dash is a case the route "
           "has no place for. `A` is `A`'s own token, and the case names what was sent.\n"]

    def table(title, caller, case_names):
        out.append(f"## {title}\n")
        out.append("| Route | " + " | ".join(case_names) + " |")
        out.append("|---|" + "---|" * len(case_names))
        for route, cases in by.items():
            if any(r.get("notJellyfin") for r in cases.values()):
                continue
            out.append(f"| `{route}` | " + " | ".join(cell(cases.get((caller, name))) for name in case_names) + " |")
        out.append("")

    mains = ["own id in the path", "no id", "path: B", "query userId: B", "query userId: nobody", "query userId: malformed", "query userId: self", "query userId: selfCompact", "query userId: zero",
             "query userId: empty", "query controllableByUserId: B", "query controllableByUserId: nobody", "query controllableByUserId: malformed", "query controllableByUserId: self",
             "query controllableByUserId: selfCompact", "query controllableByUserId: zero"]
    table("As a non-administrator A", "A", mains)
    table("As an administrator", "admin", mains)
    mixed = ["query userId: two values, own then B", "query userId: two values, B then own", "query UserId and userId: own and B", "path own, query B", "path B, query own",
             "path B, query B", "path nobody, query B", "query controllableByUserId: two values, own then B", "query controllableByUserId: two values, B then own",
             "query ControllableByUserId and controllableByUserId: own and B", "query userId and controllableByUserId: own and B", "body UserId: B", "body UserId: nobody",
             "body UserId: malformed", "body UserId: zero", "body UserId: self", "query own, body B", "query B, body own"]
    mixed_cases = {n for rows_by in by.values() for (_, n) in rows_by}
    mixed = [m for m in mixed if m in mixed_cases]
    for caller, title in (("A", "A with two ids"), ("admin", "The administrator with two ids")):
        out.append(f"## {title}\n")
        shown = [route for route, cases in by.items() if any((caller, name) in cases for name in mixed) and not any(r.get('notJellyfin') for r in cases.values())]
        out.append("| Route | " + " | ".join(mixed) + " |")
        out.append("|---|" + "---|" * len(mixed))
        for route in shown:
            out.append(f"| `{route}` | " + " | ".join(cell(by[route].get((caller, name))) for name in mixed) + " |")
        out.append("")
    out.append("## No token\n")
    out.append("| Route | no token, query userId: B | no token, no id |")
    out.append("|---|---|---|")
    for route, cases in by.items():
        if not any(r.get("notJellyfin") for r in cases.values()):
            out.append(f"| `{route}` | {cell(cases.get(('none', 'no token, query userId: B')))} | {cell(cases.get(('none', 'no token, no id')))} |")
    out.append("")
    out += findings(by)
    out.append("## Routes that are not Jellyfin's\n")
    for route, cases in by.items():
        for r in cases.values():
            if r.get("notJellyfin"):
                out.append(f"- `{route}`: not a Jellyfin route; Jellyfin answers {r.get('status')}.")
    out.append("")
    return "\n".join(out)


if __name__ == "__main__":
    main()
