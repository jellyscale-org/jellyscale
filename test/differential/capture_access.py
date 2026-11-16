#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 opens with no token, and what it refuses a non-administrator (JS-126).

Usage: python3 -I test/differential/capture_access.py <out.json>   (writes <out>.json and <out>.md; run from the repository root)

Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port over synthetic films made here with ffmpeg
(a film, an episode, a film with a text subtitle, an attached font and chapters, and a film with an image
subtitle), completes the startup wizard, makes an administrator and a non-administrator, and asks every route of
compat/endpoints.yaml with three credentials: none, a token the server never issued, and the non-administrator's.
Each credential goes in one spelling per row. No credential means none of the seven spellings, and a redirect is
recorded as its 3xx and never followed. Each row also says whether the body was the content, for the routes that
send one. The container is removed when the script ends.

compat/captures/access-12.4.md describes the result. test/differential/access_capture_test.go holds the two to
each other and to compat/endpoints.yaml.
"""
import signal
import base64
import hashlib
import http.client
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.parse

IMAGE = "jellyfin/jellyfin:12.4"
ADMIN_PASSWORD = "capture-admin-pass"
USER_PASSWORD = "capture-user-pass"
REPO = os.getcwd()
BOGUS = "0" * 8 + "feedface" * 3  # 32 hex characters no server issued

SPELLINGS = ["Authorization: MediaBrowser", "Authorization: Emby", "X-Emby-Authorization", "X-Emby-Token",
             "X-MediaBrowser-Token", "ApiKey", "api_key"]


def run(*cmd, **kw):
    return subprocess.run(cmd, check=True, capture_output=True, text=True, **kw).stdout.strip()


class Server:
    def __init__(self, port):
        self.port = port

    def request(self, method, path, headers=None, body=None, timeout=120):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        h = dict(headers or {})
        if body is not None:
            body = body.encode() if isinstance(body, str) else body
            h.setdefault("Content-Type", "application/json")
        conn.request(method, path, body=body, headers=h)
        resp = conn.getresponse()
        data = resp.read() if method != "HEAD" else b""
        out = (resp.status, {k.lower(): v for k, v in resp.getheaders()}, data)
        conn.close()
        return out

    def websocket(self, path, headers):
        """The status of a WebSocket upgrade: 101 when the server takes it."""
        sock = socket.create_connection(("127.0.0.1", self.port), timeout=30)
        lines = [f"GET {path} HTTP/1.1", f"Host: 127.0.0.1:{self.port}", "Upgrade: websocket", "Connection: Upgrade",
                 "Sec-WebSocket-Version: 13", "Sec-WebSocket-Key: " + base64.b64encode(secrets.token_bytes(16)).decode()]
        lines += [f"{k}: {v}" for k, v in headers.items()]
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        status = int(sock.recv(4096).split(b" ", 2)[1])
        sock.close()
        return status


def auth_header(token=None, device="capture-sweep"):
    parts = [f'Client="capture"', f'Device="capture"', f'DeviceId="{device}"', 'Version="1"']
    if token:
        parts.append(f'Token="{token}"')
    return "MediaBrowser " + ", ".join(parts)


def credential(spelling, token, device="capture-sweep"):
    """(headers, query) that carry token in one of the seven spellings; token None carries nothing at all."""
    if token is None:
        return {}, ""
    if spelling == "Authorization: MediaBrowser":
        return {"Authorization": auth_header(token, device)}, ""
    if spelling == "Authorization: Emby":
        return {"Authorization": auth_header(token, device).replace("MediaBrowser ", "Emby ", 1)}, ""
    if spelling == "X-Emby-Authorization":
        return {"X-Emby-Authorization": auth_header(token, device)}, ""
    if spelling in ("X-Emby-Token", "X-MediaBrowser-Token"):
        return {spelling: token}, ""
    return {}, f"{spelling}={token}"


def with_query(path, query):
    if not query:
        return path
    return path + ("&" if "?" in path else "?") + query


def ask(server, method, path, spelling, token, body=None, device="capture-sweep"):
    headers, query = credential(spelling, token, device)
    path = with_query(path, query)
    if path.startswith("/socket"):
        return server.websocket(path, headers), b"", {}
    status, h, data = server.request(method, path, headers, body)
    return status, data, h


def make_media(root):
    """Synthetic films. Returns the files whose bytes the routes send as they are."""
    font = run("fc-match", "-f", "%{file}", "serif")
    movies = os.path.join(root, "movies")
    shows = os.path.join(root, "shows")

    def ffmpeg(*args):
        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", *args], check=True)

    def poster(directory):
        ffmpeg("-f", "lavfi", "-i", "testsrc2=size=300x450:rate=1:duration=1", "-frames:v", "1", os.path.join(directory, "poster.jpg"))

    def film(directory, name, *extra_inputs_and_maps, outputs=(), seconds=12):
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, name)
        ffmpeg("-f", "lavfi", "-i", f"testsrc2=size=640x360:rate=24:duration={seconds}", "-f", "lavfi", "-i",
               f"sine=frequency=440:sample_rate=48000:duration={seconds}", *extra_inputs_and_maps,
               "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-metadata:s:a:0", "language=eng", *outputs, path)
        return path

    alpha_dir = os.path.join(movies, "Alpha Film (2021)")
    alpha = film(alpha_dir, "Alpha Film (2021).mkv", "-map", "0:v", "-map", "1:a")
    poster(alpha_dir)
    episode_dir = os.path.join(shows, "Delta Show (2020)", "Season 01")
    film(episode_dir, "Delta Show S01E01 Pilot.mkv", "-map", "0:v", "-map", "1:a")

    tmp = tempfile.mkdtemp(prefix="capture-access-")
    srt = os.path.join(tmp, "lines.srt")
    with open(srt, "w") as f:
        for n in range(3):
            f.write(f"{n + 1}\n00:00:{n * 3 + 1:02d},000 --> 00:00:{n * 3 + 3:02d},000\nEnglish line {n}\n\n")
    meta = os.path.join(tmp, "chapters.txt")
    with open(meta, "w") as f:
        f.write(";FFMETADATA1\n")
        for start, end, title in ((0, 20000, "Opening"), (20000, 45500, "Middle Part"), (45500, 60000, "")):
            f.write(f"[CHAPTER]\nTIMEBASE=1/1000\nSTART={start}\nEND={end}\ntitle={title}\n")
    sub_dir = os.path.join(movies, "Sub Font Film (2030)")
    # Streams: 0 video, 1 audio, 2 the text subtitle, 3 the font.
    film(sub_dir, "Sub Font Film (2030).mkv", "-i", srt, "-map", "0:v", "-map", "1:a", "-map", "2",
         outputs=("-c:s", "srt", "-metadata:s:s:0", "language=eng", "-attach", font, "-metadata:s:t:0", "mimetype=font/ttf"))
    poster(sub_dir)
    # Chapters on a film of their own: with the font attached, Jellyfin's ffmpeg failed to extract their pictures.
    film(os.path.join(movies, "Chapter Film (2031)"), "Chapter Film (2031).mkv", "-i", meta, "-map", "0:v", "-map", "1:a", "-map_chapters", "2", seconds=60)

    sup = os.path.join(tmp, "image.sup")
    subprocess.run(["go", "run", "./test/fixtures/pgs", sup, "1920", "1080", "2,6,660,100,600,120"], check=True, cwd=REPO)
    film(os.path.join(movies, "Pgs Film (2070)"), "Pgs Film (2070).mkv", "-i", sup, "-map", "0:v", "-map", "1:a", "-map", "2",
         outputs=("-c:s", "copy", "-metadata:s:s:0", "language=eng"))
    shutil.rmtree(tmp)
    return {"film": alpha, "font": font}


def jf_json(server, method, path, token, body=None):
    status, data, _ = ask(server, method, path, "Authorization: MediaBrowser", token, body, device="capture-admin")
    if status >= 300:
        raise SystemExit(f"{method} {path}: {status} {data[:200]!r}")
    return json.loads(data) if data else None


def sign_in(server, name, password, device):
    body = json.dumps({"Username": name, "Pw": password})
    status, h, data = server.request("POST", "/Users/AuthenticateByName", {"Authorization": auth_header(None, device)}, body)
    if status != 200:
        raise SystemExit(f"sign-in of {name}: {status} {data[:200]!r}")
    reply = json.loads(data)
    return reply["AccessToken"], reply["User"]["Id"]


def wait_for(what, check, seconds=300):
    for _ in range(seconds):
        value = check()
        if value:
            return value
        time.sleep(1)
    raise SystemExit(f"timed out waiting for {what}")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def classify(label, status, data, expect):
    """Whether the body was the content (see the docstring of access_capture_test.go)."""
    if not 200 <= status <= 299:
        return "refused"
    if label == "static stream head":
        return "no-body"
    if label in expect:
        return "same-bytes" if data and sha(data) == expect[label] else ("no-bytes" if not data else "non-empty")
    kinds = {"master playlist": b"#EXTM3U", "main playlist": b"#EXTM3U", "text subtitle": b"WEBVTT",
             "text subtitle from an offset": b"WEBVTT", "poster": b"\xff\xd8", "chapter picture": b"\xff\xd8"}
    if not data or data.lstrip()[:1] in (b"{", b"<") and label in kinds:
        return "no-bytes"
    if label in kinds and not data.startswith(kinds[label]):
        return "no-bytes"
    return "non-empty"


def main():
    out = os.path.abspath(sys.argv[1])
    work = os.path.join(os.path.expanduser("~/.cache/jellyscale/agents"), f"capture-access-{os.getpid()}")
    media = os.path.join(work, "media")
    os.makedirs(media)
    name = f"jellyscale-capture-access-{os.getpid()}"
    started = False
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))  # so a killed run still removes its container
    try:
        files = make_media(media)
        run("docker", "run", "-d", "--rm", "--name", name, "-v", f"{media}:/media:ro", "-p", "127.0.0.1::8096", IMAGE)
        started = True
        port = int(run("docker", "port", name, "8096/tcp").splitlines()[0].rsplit(":", 1)[1])
        server = Server(port)

        def up():
            try:
                status, _, data = server.request("GET", "/System/Info/Public")
                return json.loads(data) if status == 200 and b"Version" in data else None
            except (OSError, http.client.HTTPException):
                return None
        public = wait_for("jellyfin to start", up, 180)
        version = public["Version"]
        for method, path, body in (("POST", "/Startup/Configuration", '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'),
                                   ("GET", "/Startup/User", None), ("POST", "/Startup/User", json.dumps({"Name": "admin", "Password": ADMIN_PASSWORD})),
                                   ("POST", "/Startup/RemoteAccess", '{"EnableRemoteAccess":true}'), ("POST", "/Startup/Complete", None)):
            status, _, data = server.request(method, path, None, body)
            if status >= 300:
                raise SystemExit(f"{method} {path}: {status} {data[:200]!r}")
        admin_token, admin_id = sign_in(server, "admin", ADMIN_PASSWORD, "capture-admin")
        for lib, kind in (("Movies", "movies"), ("Shows", "tvshows")):
            options = {"PathInfos": [{"Path": f"/media/{lib.lower()}"}], "EnableChapterImageExtraction": True, "ExtractChapterImagesDuringLibraryScan": True}
            jf_json(server, "POST", f"/Library/VirtualFolders?refreshLibrary=false&name={lib}&collectionType={kind}", admin_token, json.dumps({"LibraryOptions": options}))
        jf_json(server, "POST", "/Library/Refresh", admin_token)

        def items():
            return jf_json(server, "GET", "/Items?Recursive=true&Fields=Path,Chapters&IncludeItemTypes=Movie,Series,Season,Episode", admin_token)["Items"]

        started_pictures = []

        def scanned():
            found = items()
            sub = [i for i in found if i["Name"].startswith("Chapter")]
            tasks = jf_json(server, "GET", "/ScheduledTasks", admin_token)
            chapters = sub[0].get("Chapters", []) if sub else []
            busy = [t["Name"] for t in tasks if t["State"] != "Idle"]
            if not busy and chapters and not all(c.get("ImageTag") for c in chapters) and not started_pictures:
                # The scan does not make them for this film on its own here; the scheduled task does (chapter-images-12.4.md).
                task = [t for t in tasks if t["Key"] == "RefreshChapterImages"][0]
                server.request("POST", f"/ScheduledTasks/Running/{task['Id']}", {"Authorization": auth_header(admin_token, "capture-admin")})
                started_pictures.append(True)
            print(f"waiting: {len(found)} items, {len(chapters)} chapters, {sum(1 for c in chapters if c.get('ImageTag'))} pictures, busy {busy}", flush=True)
            return len(found) >= 7 and chapters and all(c.get("ImageTag") for c in chapters) and not busy
        wait_for("the scan and the chapter pictures", scanned, 240)
        found = items()
        ids = {"admin": admin_id}
        for i in found:
            if i["Type"] == "Movie":
                ids[{"Alpha": "film", "Sub": "sub", "Pgs": "pgs", "Chapter": "chap"}[i["Name"].split()[0]]] = i["Id"]
            else:
                ids[i["Type"].lower()] = i["Id"]
        views = jf_json(server, "GET", "/UserViews", admin_token)["Items"]
        ids["view"] = [v["Id"] for v in views if v["Name"] == "Movies"][0]
        jf_json(server, "POST", "/Users/New", admin_token, json.dumps({"Name": "viewer", "Password": USER_PASSWORD}))
        user_token, user_id = sign_in(server, "viewer", USER_PASSWORD, "capture-sweep")
        ids["user"] = user_id
        # The token the one row that signs the user out is asked with, so the others are not turned into 401.
        logout_token, _ = sign_in(server, "viewer", USER_PASSWORD, "capture-logout")

        info = jf_json(server, "POST", f"/Items/{ids['sub']}/PlaybackInfo?userId={user_id}", admin_token, json.dumps({}))
        sub_source = info["MediaSources"][0]["Id"]
        streams = {s["Index"]: s["Type"] for s in info["MediaSources"][0]["MediaStreams"]}
        text_index = [i for i, t in streams.items() if t == "Subtitle"][0]
        attachment = 3
        pgs_info = jf_json(server, "POST", f"/Items/{ids['pgs']}/PlaybackInfo?userId={user_id}", admin_token, json.dumps({}))
        pgs_source = pgs_info["MediaSources"][0]["Id"]
        pgs_index = [s["Index"] for s in pgs_info["MediaSources"][0]["MediaStreams"] if s["Type"] == "Subtitle"][0]
        ids.update(src=info["MediaSources"][0]["Id"], subsrc=sub_source, pgssrc=pgs_source)

        film = ids["film"]
        master_query = ("MaxStreamingBitrate=1000000&MediaSourceId=" + film + "&PlaySessionId=0123456789abcdef0123456789abcdef&VideoCodec=h264&AudioCodec=aac"
                        "&MaxAudioChannels=2&TranscodingMaxAudioChannels=2&SegmentContainer=mp4&MinSegments=1&BreakOnNonKeyFrames=false&h264-profile=high,main,baseline,constrainedbaseline&h264-level=52")
        master_path = f"/Videos/{film}/master.m3u8?{master_query}"
        master = ask(server, "GET", master_path, "Authorization: MediaBrowser", admin_token)[1].decode()
        main_line = [l for l in master.splitlines() if l.startswith("main.m3u8")][0]
        main_path = f"/Videos/{film}/{main_line}"
        main = ask(server, "GET", main_path, "Authorization: MediaBrowser", admin_token)[1].decode()
        segment_line = [l for l in main.splitlines() if l and not l.startswith("#")][0]
        segment_path = f"/Videos/{film}/{segment_line}"

        body_config = json.dumps({"SubtitleMode": "Default", "PlayDefaultAudioTrack": True})
        playing = json.dumps({"ItemId": film, "MediaSourceId": film, "PositionTicks": 0, "PlayMethod": "DirectPlay", "CanSeek": True, "IsPaused": False})
        profile = json.dumps({"DeviceProfile": {"Name": "capture", "DirectPlayProfiles": [{"Container": "mkv,mp4", "Type": "Video"}],
                                                "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls"}]}})
        pref = json.dumps({"Id": "usersettings", "SortBy": "SortName", "CustomPrefs": {}})
        why = {"admin": "Jellyfin 12.4 answers this to administrators only"}

        # (method, path with the names of ids in braces, body, media label, why the non-administrator may be refused)
        # {film} {episode} {series} {season} {sub} {pgs} {user} {view} {src} {subsrc} {pgssrc} stand for what the server has.
        R = []

        def row(method, route, path, body=None, label=None, reason=None, spelling="Authorization: MediaBrowser", token=None, device=None):
            R.append(dict(method=method, route=route, path=path, body=body, label=label, reason=reason, spelling=spelling, token=token, device=device))
        u = "userId={user}"
        row("GET", "/Artists", "/Artists")
        row("GET", "/Branding/Configuration", "/Branding/Configuration")
        row("GET", "/Branding/Css", "/Branding/Css")
        row("GET", "/Branding/Css.css", "/Branding/Css.css")
        row("GET", "/DisplayPreferences/{preferenceId}", f"/DisplayPreferences/usersettings?{u}&client=capture")
        row("POST", "/DisplayPreferences/{preferenceId}", f"/DisplayPreferences/usersettings?{u}&client=capture", pref)
        row("GET", "/Genres", "/Genres")
        row("GET", "/Items", "/Items")
        row("GET", "/Items/Filters", "/Items/Filters")
        row("GET", "/Items/Filters2", "/Items/Filters2")
        row("GET", "/Items/Latest", f"/Items/Latest?{u}")
        row("GET", "/Items/Suggestions", f"/Items/Suggestions?{u}")
        row("GET", "/Items/{itemId}", "/Items/{film}")
        row("GET", "/Items/{itemId}/Ancestors", "/Items/{film}/Ancestors")
        row("GET", "/Items/{itemId}/Collections", "/Items/{film}/Collections")
        row("GET", "/Items/{itemId}/Download", "/Items/{film}/Download", label="download")
        row("GET", "/Items/{itemId}/Images/{imageType}", "/Items/{film}/Images/Primary", label="poster")
        row("GET", "/Items/{itemId}/Images/{imageType}/{imageIndex}", "/Items/{chap}/Images/Chapter/0", label="chapter picture")
        row("GET", "/Items/{itemId}/LocalTrailers", "/Items/{film}/LocalTrailers")
        row("GET", "/Items/{itemId}/PlaybackInfo", f"/Items/{{film}}/PlaybackInfo?{u}")
        row("POST", "/Items/{itemId}/PlaybackInfo", f"/Items/{{film}}/PlaybackInfo?{u}", profile)
        row("GET", "/Items/{itemId}/Similar", "/Items/{film}/Similar")
        row("GET", "/Items/{itemId}/SpecialFeatures", "/Items/{film}/SpecialFeatures")
        row("GET", "/Items/{itemId}/ThemeMedia", "/Items/{film}/ThemeMedia")
        row("GET", "/LiveTv/Programs", "/LiveTv/Programs")
        row("GET", "/LiveTv/Programs/Recommended", "/LiveTv/Programs/Recommended")
        row("GET", "/Localization/Cultures", "/Localization/Cultures")
        row("GET", "/MediaSegments/{itemId}", "/MediaSegments/{film}")
        row("GET", "/Persons", "/Persons")
        row("GET", "/Playback/BitrateTest", "/Playback/BitrateTest?size=102400")
        row("GET", "/Plugins", "/Plugins")
        row("GET", "/QuickConnect/Enabled", "/QuickConnect/Enabled")
        row("GET", "/Search/Hints", "/Search/Hints?searchTerm=film")
        row("GET", "/Sessions", "/Sessions")
        row("POST", "/Sessions/Capabilities", "/Sessions/Capabilities?playableMediaTypes=Video&supportsMediaControl=false")
        row("POST", "/Sessions/Capabilities/Full", "/Sessions/Capabilities/Full", '{"PlayableMediaTypes":["Video"],"SupportsMediaControl":false}')
        row("POST", "/Sessions/Logout", "/Sessions/Logout", token=logout_token, device="capture-logout")
        row("POST", "/Sessions/Playing", "/Sessions/Playing", playing)
        row("POST", "/Sessions/Playing/Ping", "/Sessions/Playing/Ping?playSessionId=capture")
        row("POST", "/Sessions/Playing/Progress", "/Sessions/Playing/Progress", playing)
        row("POST", "/Sessions/Playing/Stopped", "/Sessions/Playing/Stopped", playing)
        row("GET", "/Shows/NextUp", f"/Shows/NextUp?{u}")
        row("GET", "/Shows/{seriesId}/Episodes", f"/Shows/{{series}}/Episodes?{u}")
        row("GET", "/Shows/{seriesId}/Seasons", f"/Shows/{{series}}/Seasons?{u}")
        row("GET", "/socket", "/socket")
        row("GET", "/Studios", "/Studios")
        row("GET", "/SyncPlay/List", "/SyncPlay/List")
        row("GET", "/System/Configuration/encoding", "/System/Configuration/encoding")
        row("GET", "/System/Endpoint", "/System/Endpoint")
        row("GET", "/System/Info", "/System/Info")
        row("GET", "/System/Info/Public", "/System/Info/Public")
        row("GET", "/System/Ping", "/System/Ping")
        row("POST", "/System/Ping", "/System/Ping")
        row("DELETE", "/UserFavoriteItems/{itemId}", f"/UserFavoriteItems/{{film}}?{u}")
        row("POST", "/UserFavoriteItems/{itemId}", f"/UserFavoriteItems/{{film}}?{u}")
        row("GET", "/UserItems/Resume", f"/UserItems/Resume?{u}")
        row("POST", "/UserItems/{itemId}/UserData", f"/UserItems/{{film}}/UserData?{u}", '{"IsFavorite":false}')
        row("DELETE", "/UserPlayedItems/{itemId}", f"/UserPlayedItems/{{film}}?{u}")
        row("POST", "/UserPlayedItems/{itemId}", f"/UserPlayedItems/{{film}}?{u}")
        row("POST", "/Users/AuthenticateByName", "/Users/AuthenticateByName", "SIGNIN", device="capture-signin")
        row("POST", "/Users/Configuration", f"/Users/Configuration?{u}", body_config)
        row("GET", "/Users/Me", "/Users/Me")
        row("GET", "/Users/Public", "/Users/Public")
        row("GET", "/Users/{userId}", "/Users/{user}")
        row("POST", "/Users/{userId}/Configuration", "/Users/{user}/Configuration", body_config)
        row("DELETE", "/Users/{userId}/FavoriteItems/{itemId}", "/Users/{user}/FavoriteItems/{film}")
        row("POST", "/Users/{userId}/FavoriteItems/{itemId}", "/Users/{user}/FavoriteItems/{film}")
        row("GET", "/Users/{userId}/GroupingOptions", "/Users/{user}/GroupingOptions")
        row("GET", "/Users/{userId}/Items", "/Users/{user}/Items")
        row("GET", "/Users/{userId}/Items/Latest", "/Users/{user}/Items/Latest")
        row("GET", "/Users/{userId}/Items/Resume", "/Users/{user}/Items/Resume")
        row("GET", "/Users/{userId}/Items/Root", "/Users/{user}/Items/Root")
        row("GET", "/Users/{userId}/Items/{itemId}", "/Users/{user}/Items/{film}")
        row("GET", "/Users/{userId}/Items/{itemId}/Intros", "/Users/{user}/Items/{film}/Intros")
        row("GET", "/Users/{userId}/Items/{itemId}/LocalTrailers", "/Users/{user}/Items/{film}/LocalTrailers")
        row("GET", "/Users/{userId}/Items/{itemId}/SpecialFeatures", "/Users/{user}/Items/{film}/SpecialFeatures")
        row("DELETE", "/Users/{userId}/PlayedItems/{itemId}", "/Users/{user}/PlayedItems/{film}")
        row("POST", "/Users/{userId}/PlayedItems/{itemId}", "/Users/{user}/PlayedItems/{film}")
        row("GET", "/UserViews", f"/UserViews?{u}")
        row("GET", "/UserViews/GroupingOptions", "/UserViews/GroupingOptions")
        row("DELETE", "/Videos/ActiveEncodings", "/Videos/ActiveEncodings?deviceId=capture-sweep&playSessionId=capture")
        row("GET", "/Videos/{itemId}/hls1/main/{segmentId}", segment_path.replace(film, "{film}"), label="segment")
        row("GET", "/Videos/{itemId}/main.m3u8", main_path.replace(film, "{film}"), label="main playlist")
        row("GET", "/Videos/{itemId}/master.m3u8", master_path.replace(film, "{film}"), label="master playlist")
        row("GET", "/Videos/{itemId}/{file}", "/Videos/{film}/stream?static=true", label="static stream")
        row("HEAD", "/Videos/{itemId}/{file}", "/Videos/{film}/stream?static=true", label="static stream head")
        row("GET", "/Videos/{itemId}/{file}", "/Videos/{film}/stream.mkv?VideoCodec=h264&AudioCodec=aac", label="stream.mkv")
        row("GET", "/Videos/{itemId}/{sourceId}/Attachments/{index}", f"/Videos/{{sub}}/{{subsrc}}/Attachments/{attachment}", label="font")
        row("GET", "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}", f"/Videos/{{sub}}/{{subsrc}}/Subtitles/{text_index}/Stream.vtt", label="text subtitle")
        row("GET", "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}", f"/Videos/{{pgs}}/{{pgssrc}}/Subtitles/{pgs_index}/Stream.sup", label="image subtitle")
        row("GET", "/Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}", f"/Videos/{{sub}}/{{subsrc}}/Subtitles/{text_index}/10000000/Stream.vtt", label="text subtitle from an offset")

        with open(files["film"], "rb") as f:
            film_sha = sha(f.read())
        with open(files["font"], "rb") as f:
            font_sha = sha(f.read())
        expect = {"static stream": film_sha, "download": film_sha, "font": font_sha}

        # Why a row is not what the sweep expects of a served user: asked of this table, not left to the reader.
        WHY = {
            ("GET", "/Plugins"): "Jellyfin 12.4 lists plugins to administrators only: 403 to the non-administrator",
            ("GET", "/socket"): "101, not a refusal: Jellyfin takes the WebSocket upgrade for the non-administrator's token (in Authorization or ApiKey) and answers 403 with none; a plain GET with the token is 404",
            ("POST", "/Users/AuthenticateByName"): "no_token is 400, not 401: with no Authorization header at all the request names no device, which Jellyfin's sign-in needs; a token (even one never issued) in the header gets the sign-in served",
        }

        me = lambda: ask(server, "GET", "/Users/Me", "Authorization: MediaBrowser", user_token)[0]
        me_before = me()
        rows = []
        for r in R:
            path = r["path"]
            for key, value in ids.items():
                path = path.replace("{" + key + "}", value)
            asks = r["path"]
            for key in ("film", "episode", "series", "season", "sub", "pgs", "chap", "view", "src", "subsrc", "pgssrc"):
                asks = asks.replace("{" + key + "}", "{id}")
            asks = asks.replace("{user}", "{userId}")
            asks = asks.replace(film, "{id}")
            signin = r["body"] == "SIGNIN"
            body = json.dumps({"Username": "admin", "Pw": ADMIN_PASSWORD}) if signin else r["body"]
            row_out = dict(key=f"{r['method']} {asks}", method=r["method"], route=r["route"], asks=asks, token_spelling=r["spelling"])
            bodies, bodies_seen = {}, {}
            for name, token in (("no_token", None), ("unknown_token", BOGUS), ("user_token", r["token"] or user_token)):
                status, data, _ = ask(server, r["method"], path, r["spelling"], token, body, device=r["device"] or "capture-sweep")
                row_out[name] = status
                bodies[name] = classify(r["label"], status, data, expect) if r["label"] else None
            if r["label"]:
                row_out["media"] = r["label"]
                row_out["bodies"] = bodies
            reason = WHY.get((r["method"], r["route"]))
            plain = lambda status: 200 <= status <= 399 or status in (401, 403)
            if reason:
                row_out["why"] = reason
            elif not 200 <= row_out["user_token"] <= 299 or not plain(row_out["no_token"]):
                raise SystemExit(f"{row_out['key']}: {row_out['no_token']} {row_out['unknown_token']} {row_out['user_token']} and nothing says why")
            rows.append(row_out)
            print(f"{row_out['key']}: {row_out['no_token']} {row_out['unknown_token']} {row_out['user_token']}", flush=True)
        me_after = me()
        capture = dict(image=IMAGE, server_version=version, me_before=me_before, me_after=me_after, rows=rows)
        capture["open_without_token"] = sorted(r["key"] for r in rows if 200 <= r["no_token"] <= 399)
        capture["refused_to_user"] = sorted(r["key"] for r in rows if r["user_token"] in (401, 403))
        with open(out, "w") as f:
            json.dump(capture, f, indent=1)
            f.write("\n")
        with open(re.sub(r"\.json$", ".md", out), "w") as f:
            f.write(markdown(capture))
    finally:
        if started:
            remove_container(name)
        shutil.rmtree(work, ignore_errors=True)


def remove_container(name):
    """Removes the container and makes sure it is gone: a removal that failed silently left two running."""
    for _ in range(10):
        result = subprocess.run(["docker", "rm", "-fv", name], capture_output=True, text=True)
        if subprocess.run(["docker", "inspect", name], capture_output=True).returncode != 0:
            return
        time.sleep(1)
    raise SystemExit(f"could not remove the container {name}: {result.stderr.strip()}")


def markdown(c):
    rows = c["rows"]
    out = [f"# What Jellyfin 12.4 opens with no token\n",
           f"Observed with `{c['image']}` (server version {c['server_version']}), a disposable container on a Docker-assigned port over synthetic films "
           "made by `test/differential/capture_access.py`: a film, an episode of a show, a film with a text subtitle and an attached font, a film with "
           "an image subtitle (PGS), and a film with three chapters whose pictures were extracted. Two users: an administrator and a non-administrator "
           "(`viewer`, made with `POST /Users/New`). Evidence in `access-12.4.json`; the script is rerun with "
           "`python3 -I test/differential/capture_access.py compat/captures/access-12.4.json` from the repository root.\n",
           "Every route of `compat/endpoints.yaml` is asked with three credentials, each in one spelling per row: no credential at all (none of the seven "
           "spellings; a redirect would be recorded as its 3xx and not followed), a token the server never issued, and the non-administrator's token. Each row "
           f"asks for something the server has. `GET /Users/Me` with the non-administrator's token answered {c['me_before']} before and {c['me_after']} after "
           "the sweep: `POST /Sessions/Logout` and the sign-in were asked with tokens of their own, so no later row is a 401 of a signed-out token.\n",
           "## What the media routes sent\n",
           "| Route | No token | Unknown token | Non-administrator |", "|---|---|---|---|"]
    for r in rows:
        if r.get("media"):
            out.append(f"| {r['media']} (`{r['key'].split('?')[0]}`) | " + " | ".join(f"{r[k]}, {r['bodies'][k]}" for k in ("no_token", "unknown_token", "user_token")) + " |")
    out += ["", "`same-bytes` is byte for byte the film file or the font; `non-empty` a body of the route's kind; `refused` a status that is not 2xx; "
            "`no-body` a HEAD answer.\n", "## Open with no token\n",
            "The rows Jellyfin answers 2xx or 3xx with no credential at all:\n"]
    out += [f"- `{k}`" for k in c["open_without_token"]]
    out += ["", "## Refused to the non-administrator\n", "The rows Jellyfin answers 401 or 403 to the non-administrator's token, each with the reason in `access-12.4.json`:\n"]
    out += [f"- `{k}`" for k in c["refused_to_user"]]
    out += ["", "## Other findings\n"]
    for r in rows:
        if r.get("why"):
            out.append(f"- `{r['key']}`: {r['why']}.")
    differ = [r["key"] for r in rows if r["no_token"] != r["unknown_token"]]
    out += [f"- A token that was never issued got the same status as no token on every row but {differ}.",
            "- Not observed here: any signed-in user reading every account (`GET /Users`, `UserController.cs:91` and `:127` at tag v12.4); that is read from source.", ""]
    return "\n".join(out)


if __name__ == "__main__":
    main()
