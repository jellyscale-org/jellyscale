#!/usr/bin/env python3
"""Seam test for remux, where video is copied and cuts must land on source keyframes.

Run A remuxes from 0. Run B restarts at the keyframe that begins A's segment N,
with both streams copied. Segment lengths vary, so the restart point comes from
A's playlist, standing in for the persisted keyframe index.

Usage: remux.py <workdir>
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path


def run(*args):
    done = subprocess.run(args, capture_output=True, text=True)
    if done.returncode:
        raise RuntimeError(done.stderr[-400:])
    return done.stdout


def pts(playlist, stream):
    out = run("ffprobe", "-v", "error", "-allowed_extensions", "ALL", "-select_streams", stream,
              "-show_entries", "packet=pts_time", "-of", "json", str(playlist))
    return sorted(float(p["pts_time"]) for p in json.loads(out)["packets"] if "pts_time" in p)


def remux(src, out, ss, start):
    shutil.rmtree(out, ignore_errors=True)
    out.mkdir(parents=True)
    # ffmpeg's input seek lands up to about a second early, so B can begin
    # before the wanted keyframe. relabel() sorts that out afterwards.
    run("ffmpeg", "-nostdin", "-v", "error", "-y", "-copyts", "-ss", f"{ss:.6f}", "-i", str(src),
        "-map", "0:v:0", "-map", "0:a:0", "-c", "copy", "-output_ts_offset", "100",
        "-f", "hls", "-hls_time", "4", "-hls_playlist_type", "vod", "-hls_list_size", "0",
        "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
        "-hls_segment_options", "use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont",
        "-start_number", str(start), "-hls_segment_filename", str(out / "%d.m4s"), str(out / "index.m3u8"))


def first_keyframe(run_dir, segment):
    data = (run_dir / "init.mp4").read_bytes() + segment.read_bytes()
    out = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,flags",
                          "-of", "csv=p=0", "-"], input=data, capture_output=True).stdout.decode()
    return min(float(l.split(",")[0]) for l in out.splitlines() if ",K" in l)


def relabel(b, index):
    """Rename B's segments by their first keyframe, using the keyframe index.

    Whatever ffmpeg's seek did, a segment's identity comes from where it starts,
    so extra segments before the wanted one simply find their number and are
    discarded by the caller.
    """
    found = {}
    for path in b.glob("*.m4s"):
        pts = first_keyframe(b, path)
        number = min(range(len(index)), key=lambda i: abs(index[i] - pts))
        if abs(index[number] - pts) > 0.0025:
            raise RuntimeError(f"{path.name} starts at {pts}, not in the index")
        found[number] = path
    for number, path in found.items():
        path.rename(b / f"tmp{number}")
    for number in found:
        (b / f"tmp{number}").rename(b / f"{number}.m4s")
    return sorted(found)


def worst(x, y):
    if len(x) != len(y):
        return "n/a"
    return f"{max(abs(p - q) for p, q in zip(x, y)) * 1000:.2f}"


def main():
    work = Path(sys.argv[1])
    work.mkdir(parents=True, exist_ok=True)
    src = work / "src.mkv"
    if not src.exists():
        run("ffmpeg", "-nostdin", "-v", "error", "-y",
            "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24000/1001:duration=60",
            "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60",
            "-c:v", "libx264", "-preset", "veryfast", "-x264-params", "keyint=120:min-keyint=30:scenecut=40",
            "-c:a", "aac", "-b:a", "128k", str(src))

    a = work / "a"
    remux(src, a, 0, 0)
    durations = [float(l[8:].rstrip(",")) for l in (a / "index.m3u8").read_text().splitlines() if l.startswith("#EXTINF")]
    print(f"run A: {len(durations)} segments, length {min(durations):.2f}s to {max(durations):.2f}s")

    keyframes = run("ffprobe", "-v", "error", "-select_streams", "v:0", "-skip_frame", "nokey",
                    "-show_entries", "frame=pts_time", "-of", "csv=p=0", str(src)).split()
    keyframes = [float(k.strip(",")) for k in keyframes if k.strip(",")]

    for restart in (1, 3, 5):
        start_s = sum(durations[:restart])
        keyframe = min(keyframes, key=lambda k: abs(k - start_s))
        b = work / f"b{restart}"
        remux(src, b, keyframe, restart)
        index = [first_keyframe(a, a / f"{i}.m4s") for i in range(len(durations))]
        numbers = relabel(b, index)
        print(f"restart {restart}: B produced segments {numbers[0]} to {numbers[-1]}")

        spliced = work / f"spliced{restart}"
        shutil.rmtree(spliced, ignore_errors=True)
        spliced.mkdir()
        shutil.copy(a / "init.mp4", spliced)
        body = ""
        for i in range(len(durations)):
            source = a if i < restart else b
            if not (source / f"{i}.m4s").exists():
                print(f"restart {restart}: B has no segment {i}")
                continue
            shutil.copy(source / f"{i}.m4s", spliced)
            body += f"#EXTINF:{durations[i]:.6f},\n{i}.m4s\n"
        (spliced / "index.m3u8").write_text(
            "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n"
            f'#EXT-X-MAP:URI="init.mp4"\n{body}#EXT-X-ENDLIST\n')

        video_a, video_s = pts(a / "index.m3u8", "v:0"), pts(spliced / "index.m3u8", "v:0")
        audio_a, audio_s = pts(a / "index.m3u8", "a:0"), pts(spliced / "index.m3u8", "a:0")
        same_init = (a / "init.mp4").read_bytes() == (b / "init.mp4").read_bytes()
        warnings = subprocess.run(["ffmpeg", "-nostdin", "-v", "warning", "-allowed_extensions", "ALL", "-i",
                                   str(spliced / "index.m3u8"), "-f", "null", "-"], capture_output=True, text=True).stderr
        print(f"restart {restart} at {keyframe:.3f}s: video {len(video_a)}/{len(video_s)} worst {worst(video_a, video_s)}ms, "
              f"audio {len(audio_a)}/{len(audio_s)} worst {worst(audio_a, audio_s)}ms, init same={same_init}, "
              f"warnings={len([l for l in warnings.splitlines() if l.strip()])}")


if __name__ == "__main__":
    main()
