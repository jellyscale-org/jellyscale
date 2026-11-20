#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 gives the details page's Audio menu, and what jellyfin-web does with it (JS-147).

Usage: python3 -I test/differential/capture_audio_menu.py <workdir> <out.json>
Environment: PLAYWRIGHT_DIR, a directory with playwright-core installed (default ~/.cache/jellyscale/pw);
CHROMIUM_PATH, a browser that plays H.264 and AAC (default /usr/bin/google-chrome-stable).

Makes four tiny films with ffmpeg in <workdir> (synthetic names, 640x360, 20 s, one sine tone per track: 440 Hz for track 1,
550 Hz for 2, and so on), starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port with no metadata provider,
completes the wizard and, for viewers who saved no audio language and who saved German (PlayDefaultAudioTrack false),
records GET /Items/{id} (every audio stream and the source's DefaultAudioStreamIndex) and then the real jellyfin-web's
details page (capture_audio_menu_web.mjs): its Audio menu, what pressing Play after picking an entry sends and hears, and
whether the pick is selected on return. The container is removed when the script ends. The Go replay is
compat/audio_menu_capture_test.go.
"""
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import capture_user_configuration as c  # noqa: E402

workdir, out = sys.argv[1], sys.argv[2]
here = os.path.dirname(os.path.abspath(__file__))

# (name, scenario, tracks as (language, default); a language of None is an untagged track.) Babel's audio is the one of
# test/fixtures/babel.sh, so the e2e scenario's labels are the ones recorded here; Four has two tracks in one language,
# an untagged one and the default flag on the third.
FILMS = [
    ("Menu Babel", "the audio of test/fixtures/babel.sh: eng (default), deu, fra, untagged", [("eng", 1), ("deu", 0), ("fra", 0), (None, 0)]),
    ("Menu Four", "two tracks in one language, one untagged, the default flag not on the first", [("eng", 0), ("deu", 0), (None, 1), ("deu", 0)]),
    ("Menu Mono", "one audio track", [("eng", 1)]),
    ("Menu Silent", "no audio track", []),
]
SETTINGS = [("", True), ("deu", False)]


def make(name, tracks):
    command = ["ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=20"]
    for n in range(len(tracks)):
        command += ["-f", "lavfi", "-i", f"sine=frequency={440 + 110 * n}:sample_rate=48000:duration=20"]
    command += ["-map", "0:v"] + [x for n in range(len(tracks)) for x in ("-map", f"{n + 1}:a")]
    command += ["-c:v", "libx264", "-preset", "ultrafast", "-g", "48"]
    if tracks:
        command += ["-c:a", "aac"]
    for n, (language, default) in enumerate(tracks):
        if language:
            command += [f"-metadata:s:a:{n}", f"language={language}"]
        command += [f"-disposition:a:{n}", "default" if default else "0"]
    path = os.path.join(workdir, "movies", f"{name} (2020)")
    os.makedirs(path, exist_ok=True)
    subprocess.run(command + [os.path.join(path, f"{name} (2020).mkv")], check=True)


for name, _, tracks in FILMS:
    make(name, tracks)

container = subprocess.check_output(["docker", "run", "-d", "-p", "127.0.0.1::8096", "-v", f"{workdir}/movies:/media/movies:ro", c.IMAGE], text=True).strip()
try:
    port = subprocess.check_output(["docker", "port", container, "8096/tcp"], text=True).split(":")[-1].strip()
    base = f"http://127.0.0.1:{port}"
    jf = c.Jellyfin(base)
    c.wait_ready(jf)
    c.wizard(jf, "admin")
    admin, _, _ = c.sign_in(jf, "admin")
    assert jf.call("POST", "/Users/New", admin, c.j({"Name": "viewer", "Password": c.PASSWORD}))["status"] == 200
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
    assert len(ids) == len(FILMS), sorted(ids)
    time.sleep(8)  # The probe of each film finishes after the item is listed.

    config = user["Configuration"]

    def save(language, play):
        body = {**config, "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play}
        if language == "":
            body.pop("AudioLanguagePreference")
        assert jf.call("POST", f"/Users/{uid}/Configuration", token, c.j(body))["status"] == 204

    items = []
    for language, play in SETTINGS:
        save(language, play)
        for name, scenario, tracks in FILMS:
            item = jf.call("GET", f"/Items/{ids[name]}?userId={uid}", token)["json"]
            source = item["MediaSources"][0]
            items.append({
                "film": name, "AudioLanguagePreference": language, "PlayDefaultAudioTrack": play,
                "DefaultAudioStreamIndex": source.get("DefaultAudioStreamIndex", "<absent>"),
                "audio": [{"Index": s["Index"], "DisplayTitle": s.get("DisplayTitle"), "IsDefault": s["IsDefault"], "Language": s.get("Language")}
                          for s in source["MediaStreams"] if s["Type"] == "Audio"],
                "itemAudioIndexes": [s["Index"] for s in item["MediaStreams"] if s["Type"] == "Audio"],
            })
    env = dict(os.environ, BASE=base, PASSWORD=c.PASSWORD, ADMIN_TOKEN=admin, IDS=json.dumps(ids), SETTINGS=json.dumps(SETTINGS))
    env.setdefault("CHROMIUM_PATH", "/usr/bin/google-chrome-stable")
    pw = os.environ.get("PLAYWRIGHT_DIR", os.path.expanduser("~/.cache/jellyscale/pw"))
    web = subprocess.run(["node", os.path.join(here, "capture_audio_menu_web.mjs")], cwd=pw, env=env, capture_output=True, text=True)
    if web.returncode != 0:
        raise SystemExit(f"capture_audio_menu_web.mjs failed:\n{web.stdout}\n{web.stderr}")
    record = {"image": c.IMAGE, "version": "12.4.0", "date": time.strftime("%Y-%m-%d"),
              "films": [{"name": n, "scenario": s, "audio": [{"Language": l, "IsDefault": bool(d)} for l, d in t]} for n, s, t in FILMS],
              "items": items, "web": json.loads(web.stdout)}
    with open(out, "w") as f:
        json.dump(record, f, indent=1)
finally:
    subprocess.run(["docker", "rm", "-fv", container], capture_output=True)
