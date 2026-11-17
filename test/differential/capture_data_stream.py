#!/usr/bin/env python3
"""Records what a real Jellyfin 12.4 lists for a data track (JS-119).

Usage: python3 test/differential/capture_data_stream.py <media_dir> <out.json>

<media_dir> holds one MP4 with a timecode (tmcd) track, made by
`ffmpeg -f lavfi -i testsrc2=size=160x90:rate=24:duration=3 -f lavfi -i sine=duration=3 -c:v libx264 -c:a aac -timecode 01:00:00:00`.
Starts jellyfin/jellyfin:12.4 in Docker on a Docker-assigned port, with no metadata provider, completes the wizard,
adds <media_dir> as a library, and records what PlaybackInfo and GET /Items/{itemId} list as MediaStreams for the film.
The container is removed when the script ends.
"""
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import capture_user_configuration as c  # noqa: E402

media, out = sys.argv[1], sys.argv[2]
container = subprocess.check_output(["docker", "run", "-d", "-p", "127.0.0.1::8096", "-v", f"{media}:/media/movies:ro", c.IMAGE], text=True).strip()
try:
    port = subprocess.check_output(["docker", "port", container, "8096/tcp"], text=True).split(":")[-1].strip()
    jf = c.Jellyfin(f"http://127.0.0.1:{port}")
    c.wait_ready(jf)
    c.wizard(jf, "admin")
    token, uid, _ = c.sign_in(jf, "admin")
    library = {"LibraryOptions": {"EnableInternetProviders": False, "PathInfos": [{"Path": "/media/movies"}],
                                  "TypeOptions": [{"Type": "Movie", "MetadataFetchers": [], "ImageFetchers": []}]}}
    jf.call("POST", "/Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=true", token, c.j(library))
    film = None
    for _ in range(90):
        items = jf.call("GET", f"/Items?userId={uid}&Recursive=true&IncludeItemTypes=Movie", token)["json"]["Items"]
        if items:
            film = items[0]["Id"]
            break
        time.sleep(2)
    assert film, "the film was not scanned: " + subprocess.run(["docker", "logs", "--tail", "15", container], capture_output=True, text=True).stdout
    time.sleep(3)
    record = {"image": c.IMAGE, "film": film}
    record["PlaybackInfo POST"] = jf.call("POST", f"/Items/{film}/PlaybackInfo?UserId={uid}", token, "{}")["json"]
    record["PlaybackInfo GET"] = jf.call("GET", f"/Items/{film}/PlaybackInfo?UserId={uid}", token)["json"]
    record["GET /Items/{itemId}"] = jf.call("GET", f"/Items/{film}?userId={uid}", token)["json"]
    record["GET /Users/{userId}/Items/{itemId}"] = jf.call("GET", f"/Users/{uid}/Items/{film}", token)["json"]
    with open(out, "w") as f:
        json.dump(record, f, indent=1)
finally:
    subprocess.run(["docker", "rm", "-fv", container], capture_output=True)
