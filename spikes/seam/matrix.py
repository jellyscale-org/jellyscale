#!/usr/bin/env python3
"""Seam test matrix across containers, encoders and source types.

For each case, encodes a full run A and a restarted run B, splices A's segments
before the restart with B's after it, then measures:
  video:  per-frame PTS of the splice against run A
  audio:  gap or overlap at the seam, and a click score
  init:   whether fMP4 init segments match byte for byte

Click score is the largest sample-to-sample step within 200ms of the seam,
divided by the largest step in the rest of the stream. A clean tone scores
about 1; a score well above that is an audible discontinuity candidate.

Usage: matrix.py <workdir> [case_substring]
"""
import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path

import numpy as np

SEG = 4
LAST = 10
TS_OFFSET = 100
WARMUP_FRAMES = 4
REORDER_FRAMES = 2
FRAME_TIMES = {}
RATE = 48000

ENCODERS = {
    "x264": ["-c:v", "libx264", "-preset", "veryfast"],
    "x265": ["-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "log-level=error:bframes=2:b-adapt=0:open-gop=0"],
    "nvenc": ["-c:v", "h264_nvenc", "-preset", "p4", "-bf", "2", "-forced-idr", "1"],
    "hevc_nvenc": ["-c:v", "hevc_nvenc", "-preset", "p4", "-bf", "2", "-forced-idr", "1", "-tag:v", "hvc1"],
    "vaapi": ["-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-bf", "2"],
}
PRE_INPUT = {"vaapi": ["-vaapi_device", "/dev/dri/by-path/pci-0000:5c:00.0-render"]}

SOURCES = {
    "ntsc": ["-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24000/1001:duration=60",
             "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60"],
    "pal": ["-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25:duration=60",
            "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60"],
    "a441": ["-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24000/1001:duration=60",
             "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=60"],
}


def run(*args):
    done = subprocess.run(args, capture_output=True, text=True)
    if done.returncode:
        raise RuntimeError(done.stderr.strip()[-400:] or "command failed")
    return done.stdout


def make_source(work, name):
    path = work / f"src-{name}.mkv"
    if path.exists():
        return path
    if name == "vfr":
        # Drops an irregular set of frames, like screen captures and some web rips.
        run("ffmpeg", "-nostdin", "-v", "error", "-y", *SOURCES["ntsc"],
            "-vf", "select='not(eq(mod(n,7),3))*not(eq(mod(n,11),5))',setpts=PTS",
            "-fps_mode", "vfr", "-c:v", "libx264", "-preset", "veryfast", "-g", "240",
            "-c:a", "flac", str(path))
    elif name == "offset":
        base = make_source(work, "ntsc")
        run("ffmpeg", "-nostdin", "-v", "error", "-y", "-i", str(base), "-c", "copy",
            "-output_ts_offset", "7.5", str(path))
    else:
        run("ffmpeg", "-nostdin", "-v", "error", "-y", *SOURCES[name],
            "-c:v", "libx264", "-preset", "veryfast", "-g", "240", "-c:a", "flac", str(path))
    return path


def start_time(src):
    out = run("ffprobe", "-v", "error", "-show_entries", "format=start_time", "-of", "csv=p=0", str(src))
    return float(out.strip() or 0)


def frame_duration(src):
    out = run("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
              "stream=r_frame_rate", "-of", "csv=p=0", str(src)).strip().rstrip(",")
    num, den = out.split("/")
    return int(den) / int(num)


def first_frame_at(src, ss):
    """PTS of the first source frame at or after ss."""
    if src not in FRAME_TIMES:
        out = run("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
                  "packet=pts_time", "-of", "csv=p=0", str(src))
        FRAME_TIMES[src] = sorted(float(t.strip(",")) for t in out.split() if t.strip(","))
    return min(t for t in FRAME_TIMES[src] if t >= ss - 1e-6)


def encode(out, src, enc, container, ss, start):
    shutil.rmtree(out, ignore_errors=True)
    out.mkdir(parents=True)
    # AAC frames sit on a fixed 1024-sample grid. Restarted audio begins a few
    # frames early on that same grid, warming the encoder, and packets before
    # run A's cut point are dropped, so B's packets replace A's one for one.
    frame = 1024 / RATE
    base = start_time(src)
    first_frame = max(int((ss + 1e-9) // frame) - WARMUP_FRAMES, 0) if start else 0
    audio_ss = first_frame * frame
    # The HLS muxer cuts on the keyframe's DTS, which precedes its PTS by the
    # encoder's reorder delay. The first audio packet at or after that goes in.
    drop = []
    if start:
        fps_delay = REORDER_FRAMES * frame_duration(src)
        # -ss is relative to the source's start, but frame times aren't.
        keyframe = first_frame_at(src, ss + base) - base
        cut = keyframe - fps_delay
        cut_frame = int(np.ceil((cut + frame) / frame - 1e-6))
        threshold = (cut_frame - 1) * 1024 + round(base * RATE)
        drop = ["-bsf:a", f"noise=drop=lt(pts\\,{threshold})"]
    ext = "m4s" if container == "fmp4" else "ts"
    # Timestamps stay on the source's clock; offset moves it away from zero.
    args = ["ffmpeg", "-nostdin", "-v", "error", "-y", *PRE_INPUT.get(enc, []),
            "-copyts", "-ss", f"{ss}", "-i", str(src), "-ss", f"{audio_ss:.6f}", "-i", str(src),
            "-map", "0:v:0", "-map", "1:a:0", "-to", f"{base + (LAST + 1) * SEG}",
            *ENCODERS[enc], "-b:v", "2M", "-force_key_frames", f"expr:gte(t,n_forced*{SEG})",
            "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", str(RATE), *drop,
            "-output_ts_offset", str(TS_OFFSET),
            "-f", "hls", "-hls_time", str(SEG), "-hls_playlist_type", "vod", "-hls_list_size", "0",
            "-hls_segment_type", "mpegts" if container == "ts" else "fmp4",
            "-start_number", str(start), "-hls_segment_filename", str(out / f"%d.{ext}")]
    if container == "fmp4":
        # Without these the mp4 muxer rebases to 0 and records the start in an
        # edit list, so segments lose the absolute time and init files differ.
        args += ["-hls_fmp4_init_filename", "init.mp4",
                 "-hls_segment_options", "use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont"]
    run(*args, str(out / "index.m3u8"))
    return ext


def splice(work, a, b, restart, ext, container, init_from="a"):
    out = work / "spliced"
    shutil.rmtree(out, ignore_errors=True)
    out.mkdir()
    for i in range(LAST + 1):
        shutil.copy((a if i < restart else b) / f"{i}.{ext}", out)
    head = f"#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:{SEG}\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n"
    if container == "fmp4":
        shutil.copy((a if init_from == "a" else b) / "init.mp4", out)
        head += '#EXT-X-MAP:URI="init.mp4"\n'
    body = "".join(f"#EXTINF:{SEG},\n{i}.{ext}\n" for i in range(LAST + 1))
    (out / "index.m3u8").write_text(head + body + "#EXT-X-ENDLIST\n")
    return out


def frame_pts(playlist):
    out = run("ffprobe", "-v", "error", "-allowed_extensions", "ALL", "-select_streams", "v:0",
              "-show_entries", "frame=pts_time", "-of", "json", str(playlist))
    times = [float(f["pts_time"]) for f in json.loads(out)["frames"] if "pts_time" in f]
    return np.array(sorted(times))


def decode_audio(playlist):
    done = subprocess.run(
        ["ffmpeg", "-nostdin", "-v", "error", "-allowed_extensions", "ALL", "-i", str(playlist),
         "-map", "0:a:0", "-ac", "1", "-f", "f32le", "-acodec", "pcm_f32le", "-"],
        capture_output=True)
    return np.frombuffer(done.stdout, dtype=np.float32)


def click_score(pcm, seam_s):
    if len(pcm) < RATE:
        return float("nan")
    steps = np.abs(np.diff(pcm))
    # Decoded position is relative to the first sample, and the seam is near seam_s.
    center = int(seam_s * RATE)
    lo, hi = max(center - RATE // 5, 0), min(center + RATE // 5, len(steps))
    near = steps[lo:hi].max()
    rest = np.concatenate([steps[RATE // 2:lo], steps[hi:-RATE // 2]])
    return float(near / rest.max())


def decode_warnings(playlist):
    done = subprocess.run(["ffmpeg", "-nostdin", "-v", "warning", "-allowed_extensions", "ALL",
                           "-i", str(playlist), "-f", "null", "-"], capture_output=True, text=True)
    return [l for l in done.stderr.splitlines() if l.strip()]


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()[:8]


def test_case(work, name, source, enc_a, enc_b, container, restart):
    root = work / name
    root.mkdir(parents=True, exist_ok=True)
    src = make_source(work, source)
    a, b = root / "a", root / "b"
    ext = encode(a, src, enc_a, container, 0, 0)
    encode(b, src, enc_b, container, restart * SEG, restart)
    out = splice(root, a, b, restart, ext, container)

    full, mixed = frame_pts(a / "index.m3u8"), frame_pts(out / "index.m3u8")
    if len(full) == len(mixed):
        worst = float(np.abs(full - mixed).max() * 1000)
    else:
        worst = float("inf")

    seam_s = restart * SEG
    clicks = click_score(decode_audio(out / "index.m3u8"), seam_s)
    baseline = click_score(decode_audio(a / "index.m3u8"), seam_s)
    warnings = decode_warnings(out / "index.m3u8")
    init = "n/a"
    if container == "fmp4":
        init = "same" if sha(a / "init.mp4") == sha(b / "init.mp4") else "DIFF"
    return {"name": name, "video_ms": worst, "frames": (len(full), len(mixed)), "click": clicks,
            "click_base": baseline, "init": init, "warnings": len(warnings)}


def cases():
    for container in ("ts", "fmp4"):
        for enc in ("x264", "nvenc", "vaapi", "x265", "hevc_nvenc"):
            if container == "ts" and enc in ("x265", "hevc_nvenc"):
                continue
            for restart in (1, 5, 9):
                yield f"{container}-{enc}-r{restart}", "ntsc", enc, enc, container, restart
        for source in ("pal", "a441", "vfr", "offset"):
            for restart in (1, 5):
                yield f"{container}-{source}-r{restart}", source, "x264", "x264", container, restart
        for enc_a, enc_b in (("x264", "nvenc"), ("nvenc", "x264")):
            yield f"{container}-mix-{enc_a}-{enc_b}-r5", "ntsc", enc_a, enc_b, container, 5


def main():
    work = Path(sys.argv[1])
    only = sys.argv[2] if len(sys.argv) > 2 else ""
    work.mkdir(parents=True, exist_ok=True)
    print(f"{'case':<30} {'video ms':>9} {'frames':>11} {'click':>6} {'base':>5} {'init':>5} {'warn':>4}", flush=True)
    for case in cases():
        if only not in case[0]:
            continue
        try:
            r = test_case(work, *case)
            print(f"{r['name']:<30} {r['video_ms']:>9.2f} {r['frames'][0]:>5}/{r['frames'][1]:<5} "
                  f"{r['click']:>6.2f} {r['click_base']:>5.2f} {r['init']:>5} {r['warnings']:>4}", flush=True)
        except RuntimeError as err:
            print(f"{case[0]:<30} ERROR {str(err)[:110]}", flush=True)


if __name__ == "__main__":
    main()
