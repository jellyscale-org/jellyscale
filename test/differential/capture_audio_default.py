#!/usr/bin/env python3
"""Records which audio track a real Jellyfin 12.4 names as DefaultAudioStreamIndex (JS-121).

Usage: python3 -I test/differential/capture_audio_default.py <workdir> <out.json>

Makes 16 tiny films with ffmpeg in <workdir> (synthetic names, 160x90, 3 s), starts jellyfin/jellyfin:12.4 in Docker on a
Docker-assigned port with no metadata provider, completes the wizard, and asks PlaybackInfo with jellyfin-web's body (the
DeviceProfile of compat/captures/transcode-12.4.json) as a user who saved each AudioLanguagePreference ('', eng, deu,
OriginalLanguage) with each PlayDefaultAudioTrack (true, false). It also asks one film with AudioStreamIndex named, and the
same cells without a DeviceProfile. The container is removed when the script ends. The Go replay is
internal/api/audio_default_integration_test.go.
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

workdir, out = sys.argv[1], sys.argv[2]
repo = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
profile = json.load(open(os.path.join(repo, "compat/captures/transcode-12.4.json")))["deviceProfile"]
# The profile of a client that plays any track itself (Fluster's mpv): every container and codec direct played, one HLS
# fallback, and no CodecProfiles at all, so no IsSecondaryAudio condition such as Chrome's two.
ANY_CLIENT = {
    "Name": "Any track", "MaxStreamingBitrate": 1000000000, "MaxStaticBitrate": 1000000000,
    "DirectPlayProfiles": [{"Type": "Video"}, {"Type": "Audio"}],
    "TranscodingProfiles": [
        {"Type": "Video", "Container": "mp4", "Protocol": "hls", "Context": "Streaming", "VideoCodec": "h264,hevc,av1,vp9",
         "AudioCodec": "aac,ac3,eac3,opus,flac,mp3", "EnableSubtitlesInManifest": False},
        {"Type": "Audio", "Container": "mp4", "Protocol": "hls", "Context": "Streaming", "AudioCodec": "aac"}],
    "ContainerProfiles": [], "CodecProfiles": [], "SubtitleProfiles": [{"Format": "srt", "Method": "External"}, {"Format": "ass", "Method": "External"}],
}

# (name, scenario, container, tracks as (language, default)); a language of None is an untagged track.
FILMS = [
    ("Audio Def First", "default flag on the first track", "mkv", [("eng", 1), ("deu", 0)]),
    ("Audio Def Second", "default flag on the second track", "mkv", [("eng", 0), ("deu", 1)]),
    ("Audio Def None", "no track flagged", "mkv", [("eng", 0), ("deu", 0)]),
    ("Audio Def All", "every track flagged", "mkv", [("eng", 1), ("deu", 1)]),
    ("Audio Ger And Deu", "a track tagged ger and one tagged deu", "mkv", [("eng", 1), ("ger", 0), ("deu", 0)]),
    ("Audio Untagged", "an untagged first track (flagged), eng second", "mkv", [(None, 1), ("eng", 0)]),
    ("Audio Und", "a track tagged und (flagged), eng second", "mp4", [("und", 1), ("eng", 0)]),
    ("Audio Two Same", "two tracks in one language", "mkv", [("eng", 1), ("deu", 0), ("deu", 0)]),
    ("Audio Single", "a single track", "mkv", [("eng", 1)]),
    ("Audio Third First", "no deu track, first track fra, eng flagged", "mkv", [("fra", 0), ("eng", 1)]),
    ("Audio None", "no audio", "mkv", []),
    ("Audio Short De", "a track tagged de", "mkv", [("eng", 1), ("de", 0)]),
    ("Audio Region De", "a track tagged de-DE", "mkv", [("eng", 1), ("de-DE", 0)]),
    ("Audio Deu Default", "the flagged track is deu", "mkv", [("deu", 1), ("eng", 0)]),
    ("Audio Dup Second", "two tracks in one language, the second flagged", "mkv", [("eng", 0), ("deu", 0), ("deu", 1)]),
    ("Audio Three Distinct", "three languages, the last flagged", "mkv", [("eng", 0), ("deu", 0), ("fra", 1)]),
    ("Audio Dup First", "two tracks in one language first, eng flagged last", "mkv", [("deu", 0), ("deu", 0), ("eng", 1)]),
    ("Audio Dup Middle", "two tracks in one language, the middle flagged", "mkv", [("eng", 0), ("deu", 1), ("deu", 0)]),
    ("Audio Four", "four languages, the last flagged", "mkv", [("eng", 0), ("fra", 0), ("spa", 0), ("deu", 1)]),
    ("Audio Named", "three tracks and a subtitle, for the named index", "mkv", [("eng", 1), ("deu", 0), ("fra", 0)]),
]
# The first four are the cells the task asks for; the rest tell how Jellyfin compares a saved language with a track's tag.
LANGUAGES = ("", "eng", "deu", "OriginalLanguage", "ger", "de", "DEU", "fra", "und", "xx-not-a-tag")
SETTINGS = [(language, play) for language in LANGUAGES for play in (True, False)]
NAMED = [("", True), ("deu", True), ("fra", False), ("deu", False), ("OriginalLanguage", True)]
# Audio Named has a subtitle at index 4; Audio Three Distinct has its flagged track last, to tell "flagged" from "first".
NAMED_FILMS = ("Audio Named", "Audio Three Distinct")


def make(name, container, tracks):
    command = ["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=3"]
    for n in range(len(tracks)):
        command += ["-f", "lavfi", "-i", f"sine=frequency={440 + 100 * n}:sample_rate=48000:duration=3"]
    subtitle = name == "Audio Named"
    if subtitle:
        with open(os.path.join(workdir, "s.srt"), "w") as f:
            f.write("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
        command += ["-i", os.path.join(workdir, "s.srt")]
    command += ["-map", "0:v"] + [x for n in range(len(tracks)) for x in ("-map", f"{n + 1}:a")]
    if subtitle:
        command += ["-map", f"{len(tracks) + 1}", "-c:s", "srt", "-metadata:s:s:0", "language=eng"]
    command += ["-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac"]
    for n, (language, default) in enumerate(tracks):
        command += [f"-metadata:s:a:{n}", f"language={language or ''}", f"-disposition:a:{n}", "default" if default else "0"]
    path = os.path.join(workdir, "movies", f"{name} (2020)")
    os.makedirs(path, exist_ok=True)
    command += [os.path.join(path, f"{name} (2020).{container}")]
    subprocess.run(command, check=True)


def probe(name, container):
    path = os.path.join(workdir, "movies", f"{name} (2020)", f"{name} (2020).{container}")
    text = subprocess.check_output(["ffprobe", "-v", "error", "-select_streams", "a", "-show_entries",
                                    "stream=index:stream_tags=language:stream_disposition=default", "-of", "json", path], text=True)
    return [{"Index": s["index"], "Language": s.get("tags", {}).get("language"), "IsDefault": bool(s["disposition"]["default"])}
            for s in json.loads(text)["streams"]]


def url_fields(url):
    if not url:
        return None
    query = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
    return {k: query[k][0] for k in ("AudioStreamIndex", "VideoCodec", "AudioCodec", "SubtitleStreamIndex", "TranscodeReasons") if k in query}


def decision(source):
    return {
        "DefaultAudioStreamIndex": source.get("DefaultAudioStreamIndex", "<absent>"),
        "SupportsDirectPlay": source.get("SupportsDirectPlay"), "SupportsDirectStream": source.get("SupportsDirectStream"),
        "SupportsTranscoding": source.get("SupportsTranscoding"), "TranscodingUrl": url_fields(source.get("TranscodingUrl")),
        "MediaStreamTypes": [s["Type"] for s in source["MediaStreams"]],
    }


for name, _, container, tracks in FILMS:
    make(name, container, tracks)

container = subprocess.check_output(["docker", "run", "-d", "-p", "127.0.0.1::8096", "-v", f"{workdir}/movies:/media/movies:ro", c.IMAGE], text=True).strip()
try:
    port = subprocess.check_output(["docker", "port", container, "8096/tcp"], text=True).split(":")[-1].strip()
    jf = c.Jellyfin(f"http://127.0.0.1:{port}")
    c.wait_ready(jf)
    c.wizard(jf, "admin")
    admin, _, _ = c.sign_in(jf, "admin")
    made = jf.call("POST", "/Users/New", admin, c.j({"Name": "viewer", "Password": c.PASSWORD}))
    assert made["status"] == 200, made
    token, uid, user = c.sign_in(jf, "viewer")
    library = {"LibraryOptions": {"EnableInternetProviders": False, "PathInfos": [{"Path": "/media/movies"}],
                                  "TypeOptions": [{"Type": "Movie", "MetadataFetchers": [], "ImageFetchers": []}]}}
    jf.call("POST", "/Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=true", admin, c.j(library))
    ids = {}
    for _ in range(120):
        items = jf.call("GET", f"/Items?userId={uid}&Recursive=true&IncludeItemTypes=Movie", token)["json"]["Items"]
        ids = {re.sub(r" \(\d{4}\)$", "", i["Name"]): i["Id"] for i in items}
        if len(ids) == len(FILMS):
            break
        time.sleep(2)
    assert len(ids) == len(FILMS), "scanned only " + str(sorted(ids)) + subprocess.run(["docker", "logs", "--tail", "15", container], capture_output=True, text=True).stdout
    time.sleep(5)

    config = user["Configuration"]

    def save(language, play):
        body = {**config, "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play}
        if language == "":
            body.pop("AudioLanguagePreference")
        reply = jf.call("POST", f"/Users/{uid}/Configuration", token, c.j(body))
        assert reply["status"] == 204, reply
        saved = jf.call("GET", "/Users/Me", token)["json"]["Configuration"]
        assert saved.get("AudioLanguagePreference", "") == language and saved["PlayDefaultAudioTrack"] == play, saved

    def ask(film, extra=None, with_profile=True, device=profile):
        body = {"UserId": uid, "StartTimeTicks": 0, "IsPlayback": True, "AutoOpenLiveStream": True, "MediaSourceId": film,
                "MaxStreamingBitrate": 2147483647, "AlwaysBurnInSubtitleWhenTranscoding": False}
        if with_profile:
            body["DeviceProfile"] = device
        body.update(extra or {})
        reply = jf.call("POST", f"/Items/{film}/PlaybackInfo?UserId={uid}", token, c.j(body))
        assert reply["status"] == 200, reply
        return reply["json"]["MediaSources"][0]

    films = []
    for name, scenario, cont, tracks in FILMS:
        films.append({"name": name, "scenario": scenario, "container": cont, "audio": probe(name, cont), "cells": [], "withoutProfile": []})
    for language, play in SETTINGS:
        save(language, play)
        for film in films:
            source = ask(ids[film["name"]])
            listed = [{"Index": s["Index"], "Language": s.get("Language"), "IsDefault": s["IsDefault"]} for s in source["MediaStreams"] if s["Type"] == "Audio"]
            # Jellyfin lists a language by its own spelling (ger as deu), so the file's tag and the listed one are both kept.
            assert [(s["Index"], s["IsDefault"]) for s in listed] == [(s["Index"], s["IsDefault"]) for s in film["audio"]], (film["name"], listed, film["audio"])
            film["listed"] = listed
            film["cells"].append({"AudioLanguagePreference": language, "PlayDefaultAudioTrack": play, **decision(source)})
            plain = ask(ids[film["name"]], with_profile=False)
            film["withoutProfile"].append({"AudioLanguagePreference": language, "PlayDefaultAudioTrack": play, **decision(plain)})

    named = []
    for film_name in NAMED_FILMS:
        film = ids[film_name]
        for language, play in NAMED:
            save(language, play)
            for wanted in (1, 2, 3, 4, 0, 99, -1, "2", None):
                extra = {} if wanted is None else {"AudioStreamIndex": wanted}
                source = ask(film, extra)
                named.append({"film": film_name, "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play, "AudioStreamIndex": wanted, **decision(source)})
    # The item the details page of jellyfin-web is built from: its Audio menu lists the item's audio streams and selects
    # MediaSources[0].DefaultAudioStreamIndex, and Play sends the menu's value as AudioStreamIndex.
    details = []
    for language, play in (("", True), ("deu", False), ("deu", True)):
        save(language, play)
        for film_name in ("Audio Def First", "Audio Deu Default", "Audio Single"):
            item = jf.call("GET", f"/Items/{ids[film_name]}?userId={uid}", token)["json"]
            source = item["MediaSources"][0]
            details.append({"film": film_name, "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play,
                            "DefaultAudioStreamIndex": source.get("DefaultAudioStreamIndex", "<absent>"),
                            "audioStreamIndexes": [x["Index"] for x in source["MediaStreams"] if x["Type"] == "Audio"]})
    # A client whose profile has no IsSecondaryAudio condition: does Jellyfin still turn a track that is not the first into a transcode?
    any_client = []
    for language, play in (("", True), ("deu", True), ("deu", False), ("OriginalLanguage", False)):
        save(language, play)
        for film in films:
            source = ask(ids[film["name"]], device=ANY_CLIENT)
            any_client.append({"film": film["name"], "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play, **decision(source)})
    any_client_named = []
    save("deu", False)
    for film_name in NAMED_FILMS:
        for wanted in (1, 2, 3):
            source = ask(ids[film_name], {"AudioStreamIndex": wanted}, device=ANY_CLIENT)
            any_client_named.append({"film": film_name, "AudioLanguagePreference": "deu", "PlayDefaultAudioTrack": False, "AudioStreamIndex": wanted, **decision(source)})
    record = {"image": c.IMAGE, "version": "12.4.0", "date": time.strftime("%Y-%m-%d"), "films": films, "named": named, "details": details,
              "anyClientProfile": ANY_CLIENT, "anyClient": any_client, "anyClientNamed": any_client_named,
              "profile": "compat/captures/transcode-12.4.json deviceProfile, in a body of UserId, StartTimeTicks 0, IsPlayback, AutoOpenLiveStream, MediaSourceId, MaxStreamingBitrate 2147483647, AlwaysBurnInSubtitleWhenTranscoding false; AudioStreamIndex only in `named`"}
    with open(out, "w") as f:
        json.dump(record, f, indent=1)
finally:
    subprocess.run(["docker", "rm", "-fv", container], capture_output=True)
