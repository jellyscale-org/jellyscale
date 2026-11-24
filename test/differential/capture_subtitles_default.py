#!/usr/bin/env python3
"""Records which subtitle track a real Jellyfin 12.4 names as DefaultSubtitleStreamIndex (JS-041).

Usage: python3 -I test/differential/capture_subtitles_default.py <workdir> <playwright_dir> <capture.json>

Reads the 19 films A1..C6 of the existing <capture.json> (compat/captures/subtitles-default-12.4.json), makes them again
with ffmpeg, makes 16 more, starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with no metadata provider,
completes the wizard, and asks PlaybackInfo (body `{}` as the first capture did, and with jellyfin-web's DeviceProfile of
compat/captures/transcode-12.4.json, whose SubtitleProfiles lack pgssub, i.e. PGS rendering off) as a user who saved each
SubtitleMode and SubtitleLanguagePreference ('', eng, deu). Then it asks for:
  - Smart, with each AudioLanguagePreference ('', eng, jpn), on 7 films            -> "smart"
  - SubtitleStreamIndex named (a track, -1, 99), each mode, '' and eng             -> "named"
  - GET /Items/{id}, each mode and language                                          -> "items"
  - a progress report naming SubtitleStreamIndex, with RememberSubtitleSelections on and off, and who it changes -> "remember"
  - what jellyfin-web's Subtitles page saves for German (playwright)                -> "german"
The 19 old films are asked again and every answer must equal the recorded one, so the fixtures are the same films. The new
answers are written into <capture.json>, which keeps every old key. The container is removed when the script ends.
Synthetic names only.
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import capture_user_configuration as c  # noqa: E402

workdir, playwright_dir, out = sys.argv[1], sys.argv[2], sys.argv[3]
repo = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
profile = json.load(open(os.path.join(repo, "compat/captures/transcode-12.4.json")))["deviceProfile"]
record = json.load(open(out))
OLD = record["films"]
MODES = ("Default", "Always", "OnlyForced", "None", "Smart")
LANGUAGES = ("", "eng", "deu")
AUDIO = ("", "eng", "jpn")


def spec_of_old(film):
    """audio, embedded and files of a film of the first capture, from its recorded listing."""
    files = [(s["Language"], s["IsDefault"], s["IsForced"], "srt") for s in film["subtitles"] if s["IsExternal"]]
    embedded = [(s["Language"], s["IsDefault"], s["IsForced"], "srt") for s in film["subtitles"] if not s["IsExternal"]]
    return {"audio": [(film["audio"], True)], "embedded": embedded, "files": files}


# name -> audio (language, default), embedded and files as (language, default, forced, codec); None is no language tag.
FILMS = {name: spec_of_old(film) for name, film in OLD.items()}
NEW = {
    # Smart on a film whose audio is English and Japanese, and on one whose first audio is Japanese.
    "S1": {"audio": [("eng", True), ("jpn", False)], "embedded": [("eng", False, False, "srt"), ("jpn", False, False, "srt"), ("deu", False, False, "srt")], "files": []},
    "S2": {"audio": [("jpn", True), ("eng", False)], "embedded": [], "files": [("deu", False, False, "srt"), ("eng", False, False, "srt")]},
    # A track tagged ger, one tagged deu (in the film and as files), untagged ones.
    "G1": {"audio": [("eng", True)], "embedded": [("ger", False, False, "srt"), ("eng", False, False, "srt")], "files": []},
    "G2": {"audio": [("eng", True)], "embedded": [("deu", False, False, "srt"), ("eng", False, False, "srt")], "files": []},
    "G3": {"audio": [("eng", True)], "embedded": [], "files": [("ger", False, False, "srt"), ("eng", False, False, "srt")]},
    "U1": {"audio": [("eng", True)], "embedded": [(None, False, False, "srt"), ("eng", False, False, "srt")], "files": []},
    "U2": {"audio": [("eng", True)], "embedded": [("eng", False, False, "srt")], "files": [(None, False, False, "srt")]},
    # PGS inside the film, beside a SubRip track, forced, and as a .sup file beside the film.
    "P1": {"audio": [("eng", True)], "embedded": [("eng", False, False, "pgs")], "files": []},
    "P2": {"audio": [("eng", True)], "embedded": [("eng", False, False, "srt"), ("deu", True, False, "pgs")], "files": []},
    "P3": {"audio": [("eng", True)], "embedded": [("eng", False, False, "pgs"), ("deu", False, True, "pgs")], "files": []},
    "P4": {"audio": [("eng", True)], "embedded": [], "files": [("deu", False, False, "srt"), ("eng", False, False, "sup")]},
    # A forced and a plain track in one language the viewer did not save (A4 is this as files): in what order does a saved language put them.
    "X1": {"audio": [("eng", True)], "embedded": [("deu", False, True, "srt"), ("deu", False, False, "srt")], "files": []},
    "X2": {"audio": [("eng", True)], "embedded": [("deu", False, False, "srt"), ("deu", False, True, "srt")], "files": []},
    "X3": {"audio": [("eng", True)], "embedded": [], "files": [("fra", False, True, "srt"), ("fra", False, False, "srt")]},
    # Two films that are one film twice, for what a progress report changes in another film.
    "R1": {"audio": [("eng", True)], "embedded": [("eng", False, False, "srt"), ("deu", False, False, "srt")], "files": []},
    "R2": {"audio": [("eng", True)], "embedded": [("eng", False, False, "srt"), ("deu", False, False, "srt")], "files": []},
}
FILMS.update(NEW)
SMART_FILMS = ("B3", "B4", "A7", "S1", "S2", "A1", "C3")
NAMED_FILMS = ("A5", "P2", "B3")
ITEM_FILMS = ("A5", "B3", "B4", "C3", "P2", "S1", "G1")
FILM_LANG = ("G1", "G2", "G3", "U1", "U2", "P1", "P2", "P3", "P4")


def srt_text():
    return "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"


def make(name, spec):
    folder = os.path.join(workdir, "movies", f"{name} (2020)")
    os.makedirs(folder, exist_ok=True)
    stem = os.path.join(folder, f"{name} (2020)")
    command = ["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=6"]
    for n in range(len(spec["audio"])):
        command += ["-f", "lavfi", "-i", f"sine=frequency={440 + 100 * n}:sample_rate=48000:duration=6"]
    first_sub = 1 + len(spec["audio"])
    for n, (_, _, _, codec) in enumerate(spec["embedded"]):
        command += ["-i", os.path.join(workdir, "sub.sup" if codec == "pgs" else "sub.srt")]
    command += ["-map", "0:v"] + [x for n in range(len(spec["audio"])) for x in ("-map", f"{n + 1}:a")]
    command += [x for n in range(len(spec["embedded"])) for x in ("-map", str(first_sub + n))]
    command += ["-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-c:s", "copy"]
    for n, (_, _, _, codec) in enumerate(spec["embedded"]):
        if codec == "srt":
            command += [f"-c:s:{n}", "srt"]
    for n, (language, default) in enumerate(spec["audio"]):
        command += [f"-metadata:s:a:{n}", f"language={language or ''}", f"-disposition:a:{n}", "default" if default else "0"]
    for n, (language, default, forced, _) in enumerate(spec["embedded"]):
        flags = "+".join(w for w, on in (("default", default), ("forced", forced)) if on) or "0"
        command += [f"-metadata:s:s:{n}", f"language={language or ''}", f"-disposition:s:{n}", flags]
    subprocess.run(command + [f"{stem}.mkv"], check=True)
    for language, default, forced, codec in spec["files"]:
        words = ".".join(w for w in (language, "default" if default else None, "forced" if forced else None) if w)
        path = f"{stem}.{words}.{codec}" if words else f"{stem}.{codec}"
        data = open(os.path.join(workdir, "sub.sup" if codec == "sup" else "sub.srt"), "rb").read()
        open(path, "wb").write(data)


def tags_of(name, spec):
    """The language tag each subtitle was made with, in Jellyfin's order: the files by file name, then the film's own tracks."""
    files = []
    for language, default, forced, codec in spec["files"]:
        words = ".".join(w for w in (language, "default" if default else None, "forced" if forced else None) if w)
        files.append((f"{name} (2020).{words}.{codec}" if words else f"{name} (2020).{codec}", language))
    return [tag for _, tag in sorted(files, key=lambda f: f[0].lower())] + [e[0] for e in spec["embedded"]]


def tracks_of(source):
    keys = ("Index", "Language", "IsDefault", "IsForced", "IsExternal", "Codec", "DeliveryMethod", "IsTextSubtitleStream")
    return [{k: s.get(k) for k in keys} for s in source["MediaStreams"] if s["Type"] == "Subtitle"]


def url_fields(url):
    if not url:
        return None
    query = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
    return {k: query[k][0] for k in ("AudioStreamIndex", "SubtitleStreamIndex", "SubtitleMethod", "VideoCodec", "TranscodeReasons") if k in query}


def answer(source):
    """What a reply says about the subtitle: the default as a number or null, how it plays, and the stream at that position."""
    default = source.get("DefaultSubtitleStreamIndex")
    at = None
    if isinstance(default, int) and 0 <= default < len(source["MediaStreams"]):
        stream = source["MediaStreams"][default]
        at = {"Type": stream["Type"], "Index": stream["Index"]}
    delivery = None
    for s in source["MediaStreams"]:
        if s["Type"] == "Subtitle" and s["Index"] == default:
            delivery = s.get("DeliveryMethod")
    return {
        "DefaultSubtitleStreamIndex": default, "DefaultAudioStreamIndex": source.get("DefaultAudioStreamIndex"),
        "SupportsDirectPlay": source.get("SupportsDirectPlay"), "SupportsDirectStream": source.get("SupportsDirectStream"),
        "TranscodingUrl": url_fields(source.get("TranscodingUrl")), "DeliveryMethod": delivery, "AtPosition": at,
    }


subprocess.run(["go", "build", "-o", os.path.join(workdir, "pgs"), "./test/fixtures/pgs"], cwd=repo, check=True)
subprocess.run([os.path.join(workdir, "pgs"), os.path.join(workdir, "sub.sup"), "1920", "1080", "1,2,660,100,600,120"], check=True)
open(os.path.join(workdir, "sub.srt"), "w").write(srt_text())
for name, spec in FILMS.items():
    make(name, spec)

container = subprocess.check_output(["docker", "run", "-d", "-p", "127.0.0.1::8096", "-v", f"{workdir}/movies:/media/movies:ro", c.IMAGE], text=True).strip()
try:
    port = subprocess.check_output(["docker", "port", container, "8096/tcp"], text=True).split(":")[-1].strip()
    base = f"http://127.0.0.1:{port}"
    jf = c.Jellyfin(base)
    c.wait_ready(jf)
    c.wizard(jf, "admin")
    admin, _, _ = c.sign_in(jf, "admin")
    library = {"LibraryOptions": {"EnableInternetProviders": False, "PathInfos": [{"Path": "/media/movies"}],
                                  "TypeOptions": [{"Type": "Movie", "MetadataFetchers": [], "ImageFetchers": []}]}}

    def new_user(name):
        made = jf.call("POST", "/Users/New", admin, c.j({"Name": name, "Password": c.PASSWORD}))
        assert made["status"] == 200, made
        return c.sign_in(jf, name)

    token, uid, user = new_user("viewer")
    jf.call("POST", "/Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=true", admin, c.j(library))
    ids = {}
    for attempt in range(180):
        if attempt % 10 == 5:
            jf.call("POST", "/Library/Refresh", admin)  # A scan that ran before the library was registered finds nothing.
        items = jf.call("GET", f"/Items?userId={uid}&Recursive=true&IncludeItemTypes=Movie", token)["json"]["Items"]
        ids = {re.sub(r" \(\d{4}\)$", "", i["Name"]): i["Id"] for i in items}
        if len(ids) == len(FILMS):
            break
        time.sleep(2)
    assert len(ids) == len(FILMS), "scanned only " + str(sorted(ids)) + subprocess.run(["docker", "logs", "--tail", "15", container], capture_output=True, text=True).stdout
    time.sleep(10)

    config = user["Configuration"]

    def save(who, mode, language, audio="", remember=True, play=True):
        who_token, who_id, who_config = who
        body = {**who_config, "SubtitleMode": mode, "SubtitleLanguagePreference": language, "AudioLanguagePreference": audio,
                "RememberSubtitleSelections": remember, "PlayDefaultAudioTrack": play}
        if audio == "":
            body.pop("AudioLanguagePreference")
        reply = jf.call("POST", f"/Users/{who_id}/Configuration", who_token, c.j(body))
        assert reply["status"] == 204, reply
        saved = jf.call("GET", "/Users/Me", who_token)["json"]["Configuration"]
        assert saved["SubtitleMode"] == mode and saved["SubtitleLanguagePreference"] == language and saved.get("AudioLanguagePreference", "") == audio and saved["PlayDefaultAudioTrack"] == play, saved

    me = (token, uid, config)

    def ask(film, who=None, extra=None, device=None):
        who_token, who_id, _ = who or me
        body = {"UserId": who_id, "StartTimeTicks": 0, "IsPlayback": True, "AutoOpenLiveStream": True, "MediaSourceId": film,
                "MaxStreamingBitrate": 2147483647, "AlwaysBurnInSubtitleWhenTranscoding": False}
        if device is None:
            body = {}
        else:
            body["DeviceProfile"] = device
        body.update(extra or {})
        reply = jf.call("POST", f"/Items/{film}/PlaybackInfo?UserId={who_id}", who_token, c.j(body))
        assert reply["status"] == 200, reply
        return reply["json"]["MediaSources"][0]

    def item(film, who=None):
        who_token, who_id, _ = who or me
        reply = jf.call("GET", f"/Items/{film}?userId={who_id}", who_token)
        assert reply["status"] == 200, reply
        return reply["json"]["MediaSources"][0]

    # 1. The 19 old films and the new ones: every mode and language, with and without the DeviceProfile.
    films = {}
    for name, spec in FILMS.items():
        source = ask(ids[name])
        listed = tracks_of(source)
        if name in OLD:
            want = [(s["Index"], s["Language"], s["IsDefault"], s["IsForced"], s["IsExternal"]) for s in OLD[name]["subtitles"]]
            got = [(s["Index"], s["Language"], s["IsDefault"], s["IsForced"], s["IsExternal"]) for s in listed]
            assert got == want, (name, got, want)
        for track, tag in zip(listed, tags_of(name, spec), strict=True):
            # Jellyfin lists a tag by its own spelling, ger as deu (audio-default-12.4.md); the tag the file wore is kept.
            assert track["Language"] == ("deu" if tag == "ger" else tag), (name, track, tag)
            track["Tag"] = tag
        films[name] = {"audio": spec["audio"][0][0] if len(spec["audio"]) == 1 else [{"Language": a, "IsDefault": d} for a, d in spec["audio"]],
                       "subtitles": listed, "results": []}
    for mode in MODES:
        for language in LANGUAGES:
            save(me, mode, language)
            for name in FILMS:
                plain, with_profile = ask(ids[name]), ask(ids[name], device=profile)
                cell = {"mode": mode, "language_preference": language, "DefaultSubtitleStreamIndex": plain.get("DefaultSubtitleStreamIndex"),
                        "withProfile": answer(with_profile)}
                if name in OLD:
                    recorded = [r for r in OLD[name]["results"] if r["mode"] == mode and r["language_preference"] == language]
                    assert len(recorded) == 1 and recorded[0]["DefaultSubtitleStreamIndex"] == cell["DefaultSubtitleStreamIndex"], (name, cell, recorded)
                films[name]["results"].append(cell)
    # The old films keep their recorded keys; the new answers are added to them.
    for name in OLD:
        OLD[name]["results"] = films[name]["results"]
    for name in NEW:
        OLD[name] = films[name]

    # 2. Smart, each language, each AudioLanguagePreference: which audio plays and which subtitle is the default. A saved audio
    # language changes the track that plays only where PlayDefaultAudioTrack is off (audio-default-12.4.md), so the matrix is
    # asked with it off on 7 films and on S1 and S2 (which have an English and a Japanese track) with it on too.
    smart = []
    for play in (False, True):
        for audio in AUDIO:
            for language in LANGUAGES:
                save(me, "Smart", language, audio, play=play)
                for name in (SMART_FILMS if not play else ("S1", "S2")):
                    plain, with_profile = ask(ids[name]), ask(ids[name], device=profile)
                    smart.append({"film": name, "AudioLanguagePreference": audio, "PlayDefaultAudioTrack": play, "language_preference": language,
                                  "DefaultSubtitleStreamIndex": plain.get("DefaultSubtitleStreamIndex"), "DefaultAudioStreamIndex": plain.get("DefaultAudioStreamIndex"),
                                  "withProfile": answer(with_profile)})

    # 3. SubtitleStreamIndex named, under each mode: a track, -1 (the details page's Off), 99, and none.
    named = []
    for mode in MODES:
        for language in ("", "eng"):
            save(me, mode, language)
            for name in NAMED_FILMS:
                indexes = [s["Index"] for s in films[name]["subtitles"]]
                for wanted in indexes + [-1, 99, None]:
                    extra = {} if wanted is None else {"SubtitleStreamIndex": wanted}
                    plain, with_profile = ask(ids[name], extra=extra), ask(ids[name], extra=extra, device=profile)
                    named.append({"film": name, "mode": mode, "language_preference": language, "SubtitleStreamIndex": wanted,
                                  "DefaultSubtitleStreamIndex": plain.get("DefaultSubtitleStreamIndex"), "withProfile": answer(with_profile)})

    # 4. GET /Items/{id}: does it carry DefaultSubtitleStreamIndex by the viewer's mode, and which tracks does it list?
    items = []
    for mode in MODES:
        for language in LANGUAGES:
            save(me, mode, language)
            for name in ITEM_FILMS:
                source = item(ids[name])
                plain = ask(ids[name])
                items.append({"film": name, "mode": mode, "language_preference": language,
                              "DefaultSubtitleStreamIndex": source.get("DefaultSubtitleStreamIndex", "<absent>"),
                              "playbackInfo": plain.get("DefaultSubtitleStreamIndex", "<absent>"),
                              "subtitleIndexes": [s["Index"] for s in source["MediaStreams"] if s["Type"] == "Subtitle"]})

    # 5. A progress report naming SubtitleStreamIndex. Each case has two fresh users, V (who reports) and W. Mode Always, no
    # language: R1 and R2 are one film twice, whose default is its first track (2) before any report.
    remember = []
    serial = 0
    for on in (True, False):
        for reported in (3, -1, 2):
            serial += 1
            v, w = new_user(f"remember{serial}v"), new_user(f"remember{serial}w")
            who_v, who_w = (v[0], v[1], v[2]["Configuration"]), (w[0], w[1], w[2]["Configuration"])
            for who in (who_v, who_w):
                save(who, "Always", "", remember=on)

            def snapshot(label):
                row = {"after": label}
                for key, film, who in (("sameViewerSameFilm", "R1", who_v), ("sameViewerOtherFilm", "R2", who_v), ("otherViewerSameFilm", "R1", who_w)):
                    row[key] = ask(ids[film], who=who).get("DefaultSubtitleStreamIndex")
                    row[key + "Item"] = item(ids[film], who=who).get("DefaultSubtitleStreamIndex", "<absent>")
                return row

            baseline = snapshot("nothing")
            source = ask(ids["R1"], who=who_v, device=profile)
            session = {"ItemId": ids["R1"], "MediaSourceId": ids["R1"], "PlaySessionId": "capture-session", "CanSeek": True, "PlayMethod": "DirectPlay",
                       "SubtitleStreamIndex": reported, "AudioStreamIndex": 1, "PositionTicks": 0, "IsPaused": False}
            steps = []
            for label, route, extra in (("start", "/Sessions/Playing", {}), ("progress", "/Sessions/Playing/Progress", {"PositionTicks": 10000000}),
                                        ("stop", "/Sessions/Playing/Stopped", {"PositionTicks": 20000000})):
                reply = jf.call("POST", route, who_v[0], c.j({**session, **extra}))
                assert reply["status"] == 204, (label, reply)
                steps.append(snapshot(label))
            remember.append({"RememberSubtitleSelections": on, "reportedSubtitleStreamIndex": reported, "mode": "Always",
                             "before": baseline, "steps": steps})

    # 6. Which language code jellyfin-web's Subtitles page saves for German, and what Jellyfin lists for German.
    cultures = jf.call("GET", "/Localization/Cultures", token)["json"]
    german = [x for x in cultures if x["TwoLetterISOLanguageName"] == "de"]
    web_user = new_user("webviewer")
    web = subprocess.run(["node", os.path.join(repo, "test/differential/capture_subtitles_default_web.mjs")], cwd=playwright_dir, capture_output=True, text=True,
                         env={**os.environ, "BASE": base, "PASSWORD": c.PASSWORD, "USERNAME": "webviewer", "CHROMIUM_PATH": os.environ.get("CHROMIUM_PATH", "/usr/bin/chromium")})
    assert web.returncode == 0, web.stderr
    saved_by_web = json.loads(web.stdout)

    record.update({
        "image": c.IMAGE, "version": "12.4.0", "date": time.strftime("%Y-%m-%d"),
        "profile": "compat/captures/transcode-12.4.json deviceProfile (SubtitleProfiles vtt, ass, ssa, all External: PGS rendering off), in a body of UserId, StartTimeTicks 0, IsPlayback, AutoOpenLiveStream, MediaSourceId, MaxStreamingBitrate 2147483647, AlwaysBurnInSubtitleWhenTranscoding false; the other cells are asked with body {}",
        "films": OLD, "smart": smart, "named": named, "items": items, "remember": remember,
        "german": {"cultures": german, "savedByTheSubtitlesPage": saved_by_web},
    })
    with open(out, "w") as f:
        json.dump(record, f, indent=1)
finally:
    subprocess.run(["docker", "rm", "-fv", container], capture_output=True)
