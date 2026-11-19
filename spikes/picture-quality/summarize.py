#!/usr/bin/env python3
"""Prints the report's tables from results/*.csv (python3 summarize.py)."""
import csv, math, os, sys
from collections import defaultdict
R = os.path.join(os.path.dirname(os.path.abspath(__file__)), "results")

def rows(name):
    path = os.path.join(R, name + ".csv")
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return list(csv.DictReader(f))

def mean(xs):
    xs = list(xs)
    return sum(xs) / len(xs) if xs else float("nan")

def table(head, lines):
    print("| " + " | ".join(head) + " |")
    print("|" + "---|" * len(head))
    for l in lines:
        print("| " + " | ".join(str(x) for x in l) + " |")
    print()

PRESETS = ["superfast", "veryfast", "faster", "fast", "medium", "slow"]
CAPS = {"4": "4 Mbps", "8": "8 Mbps", "0": "Auto"}

def presets():
    d = [r for r in rows("presets") if r["variant"] == "app"]
    if not d: return
    clips = sorted({r["clip"] for r in d})
    crfclips = sorted({r["clip"] for r in d if r["crf"] == "20"})
    def cell(sel, clipset):
        got = [r for r in d if sel(r) and r["clip"] in clipset]
        if len({r["clip"] for r in got}) != len(clipset): return "-"
        return "%.1f / %.1f / %.2f" % (mean(float(r["vmaf"]) for r in got), mean(float(r["vmaf_p5"]) for r in got), mean(float(r["kbps"]) for r in got) / 1000)
    print("### Preset by cap, CRF 23 (mean of %d clips): VMAF mean / VMAF 5th percentile frame / Mbps\n" % len(clips))
    table(["Preset"] + list(CAPS.values()),
          [[p] + [cell(lambda r, p=p, c=c: r["preset"] == p and r["crf"] == "23" and r["cap_mbps"] == c, clips) for c in CAPS] for p in PRESETS])
    print("### CRF by preset and cap (mean of the %d clips run at every CRF): VMAF mean / 5th percentile / Mbps\n" % len(crfclips))
    table(["Preset", "CRF", "4 Mbps", "Auto"],
          [[p, crf] + [cell(lambda r, p=p, c=c, crf=crf: r["preset"] == p and r["crf"] == crf and r["cap_mbps"] == c, crfclips) for c in ("4", "0")]
           for p in ["veryfast", "fast", "medium"] for crf in ["23", "20", "18"]])
    for cap in ["4", "0"]:
        print("### Each clip at %s, CRF 23: VMAF mean / 5th percentile / Mbps\n" % CAPS[cap])
        table(["Preset"] + clips,
              [[p] + [cell(lambda r, p=p: r["preset"] == p and r["crf"] == "23" and r["cap_mbps"] == cap, [c]) for c in clips] for p in PRESETS])
    print("### Each clip at Auto by CRF, preset fast: VMAF mean / 5th percentile / Mbps\n")
    table(["CRF"] + crfclips,
          [[crf] + [cell(lambda r, crf=crf: r["preset"] == "fast" and r["crf"] == crf and r["cap_mbps"] == "0", [c]) for c in crfclips] for crf in ["23", "20", "18"]])
    raw = [r for r in rows("presets") if r["variant"] == "raw20"]
    if raw:
        print("### A real 20 Mbps cap against the app's 6 bits per pixel (Auto): VMAF mean / 5th percentile / Mbps\n")
        lines = []
        for clip in sorted({r["clip"] for r in raw}):
            for p in ["fast"]:
                for crf in ["23", "20", "18"]:
                    a = [r for r in d if r["clip"] == clip and r["preset"] == p and r["crf"] == crf and r["cap_mbps"] == "0"]
                    b = [r for r in raw if r["clip"] == clip and r["preset"] == p and r["crf"] == crf]
                    f = lambda x: "%.1f / %.1f / %.2f" % (float(x[0]["vmaf"]), float(x[0]["vmaf_p5"]), float(x[0]["kbps"]) / 1000) if x else "-"
                    lines.append([clip, p, crf, f(a), f(b)])
        table(["Clip", "Preset", "CRF", "Auto (app)", "20 Mbps"], lines)

def settings():
    d = rows("settings")
    if not d: return
    for cond, title in [(("fast", "18", "4"), "CRF 18 under a 4 Mbps cap (equal bits)")]:
        got = [r for r in d if (r["preset"], r["crf"], r["cap_mbps"]) == cond]
        clips = sorted({r["clip"] for r in got})
        base = {r["clip"]: r for r in got if r["variant"] == "app"}
        print("### Settings beyond the preset, preset fast, %s: change against the app's arguments, mean of %d clips\n" % (title, len(clips)))
        lines = []
        for v in sorted({r["variant"] for r in got} - {"app"}):
            rs = [r for r in got if r["variant"] == v and r["clip"] in base]
            if len(rs) != len(base): continue
            dv = mean(float(r["vmaf"]) - float(base[r["clip"]]["vmaf"]) for r in rs)
            dp = mean(float(r["vmaf_p5"]) - float(base[r["clip"]]["vmaf_p5"]) for r in rs)
            ds = mean(float(r["psnr_y"]) - float(base[r["clip"]]["psnr_y"]) for r in rs)
            db = mean(float(r["kbps"]) / float(base[r["clip"]]["kbps"]) - 1 for r in rs) * 100
            dc = mean(float(r["cpu_s_per_s"]) / float(base[r["clip"]]["cpu_s_per_s"]) - 1 for r in rs) * 100
            per = " ".join("%+.1f" % (float(r["vmaf"]) - float(base[r["clip"]]["vmaf"])) for r in sorted(rs, key=lambda r: r["clip"]))
            lines.append([v, "%+.2f" % dv, "%+.2f" % dp, "%+.2f" % ds, "%+.0f%%" % db, "%+.0f%%" % dc, per])
        table(["Variant", "VMAF", "VMAF 5th pct", "PSNR-Y dB", "Bits", "CPU", "VMAF by clip (%s)" % ", ".join(clips)], lines)

def rate_at(points, target):
    """Mbps at which the (kbps, vmaf) curve reaches target, log-linear between points."""
    pts = sorted(points)
    for (k0, v0), (k1, v1) in zip(pts, pts[1:]):
        if v0 <= target <= v1 and v1 > v0:
            return math.exp(math.log(k0) + (math.log(k1) - math.log(k0)) * (target - v0) / (v1 - v0)) / 1000
    return None

def codecs():
    d = rows("codecs")
    if not d: return
    clips = sorted({r["clip"] for r in d})
    encs = sorted({(r["codec"], r["preset"]) for r in d}, key=lambda e: (["x264", "x265", "svtav1"].index(e[0]), e[1]))
    for target in (90, 95):
        print("### Mbps needed for VMAF %d, no cap (interpolated between CRFs), and the saving against x264 veryfast\n" % target)
        need = {}
        for e in encs:
            for c in clips:
                pts = [(float(r["kbps"]), float(r["vmaf"])) for r in d if (r["codec"], r["preset"]) == e and r["clip"] == c]
                need[e, c] = rate_at(pts, target)
        lines = []
        for e in encs:
            cells, savings = [], []
            for c in clips:
                n, b = need[e, c], need[("x264", "veryfast"), c]
                cells.append("%.2f" % n if n else "out of range")
                if n and b: savings.append(n / b - 1)
            cpu = mean(float(r["cpu_s_per_s"]) for r in d if (r["codec"], r["preset"]) == e)
            lines.append(["%s %s" % e] + cells + ["%+.0f%%" % (mean(savings) * 100) if savings else "-", "%.1f" % cpu])
        table(["Encoder"] + clips + ["Bits against x264 veryfast", "CPU s per s (under contention)"], lines)

def cost():
    for name in sorted(f[:-4] for f in os.listdir(R) if f.startswith("cost-")):
        d = rows(name)
        if not d: continue
        srcs = list(dict.fromkeys(r["source"] for r in d))
        for ff in ["host", "6.1"]:
            print("### %s, ffmpeg %s: CPU seconds per second of film (and against veryfast)\n" % (name, "9.0.2" if ff == "host" else "6.1.1"))
            lines = []
            for p in list(dict.fromkeys(r["preset"] for r in d)):
                line = [p]
                for s in srcs:
                    r = [x for x in d if x["ffmpeg"] == ff and x["source"] == s and x["preset"] == p]
                    b = [x for x in d if x["ffmpeg"] == ff and x["source"] == s and x["preset"] == "veryfast"]
                    line.append("%.2f (%.1fx)" % (float(r[0]["cpu_s_per_s"]), float(r[0]["cpu_s_per_s"]) / float(b[0]["cpu_s_per_s"])) if r and b else (r[0]["cpu_s_per_s"] if r else "-"))
                lines.append(line)
            table(["Preset"] + [s.replace("src/", "1080p ").replace("src4k/", "4K ") for s in srcs], lines)

def latency():
    for name in sorted(f[:-4] for f in os.listdir(R) if f.startswith("latency-")):
        d = rows(name)
        if not d: continue
        print("### %s: seconds from ffmpeg's start to the first segment / to the fourth (a chunk), start and seek\n" % name)
        cpusets = list(dict.fromkeys(r["cpus"] for r in d))
        lines = []
        for src in list(dict.fromkeys(r["source"] for r in d)):
            for p in list(dict.fromkeys(r["preset"] for r in d)):
                line = [src.replace("src/long1080", "1080p").replace("src4k/long4k", "4K"), p]
                for c in cpusets:
                    for start in ["0", "8"]:
                        r = [x for x in d if x["cpus"] == c and x["source"] == src and x["preset"] == p and x["start_segment"] == start]
                        line.append("%s / %s" % (r[0]["seg1_s"], r[0]["seg4_s"]) if r else "-")
                lines.append(line)
        table(["Source", "Preset"] + ["%s %s" % (c, s) for c in cpusets for s in ("start", "seek")], lines)

def steps():
    d = rows("settings")
    for preset in ["veryfast", "faster"]:
        for cap in ["0", "4"]:
            got = [r for r in d if r["preset"] == preset and r["crf"] == "23" and r["cap_mbps"] == cap]
            base = {r["clip"]: r for r in got if r["variant"] == "app"}
            if len(base) < 4: continue
            print("### One setting changed on %s, CRF 23 at %s: change, mean of %d clips\n" % (preset, CAPS[cap], len(base)))
            lines = []
            for v in sorted({r["variant"] for r in got} - {"app"}):
                rs = [r for r in got if r["variant"] == v and r["clip"] in base]
                if len(rs) != len(base): continue
                f = lambda k: mean(float(r[k]) - float(base[r["clip"]][k]) for r in rs)
                g = lambda k: mean(float(r[k]) / float(base[r["clip"]][k]) - 1 for r in rs) * 100
                lines.append([v, "%+.2f" % f("vmaf"), "%+.2f" % f("vmaf_p5"), "%+.0f%%" % g("kbps"), "%+.0f%%" % g("cpu_s_per_s")])
            table(["Variant", "VMAF", "VMAF 5th pct", "Bits", "CPU"], lines)

which = sys.argv[1:] or ["presets", "settings", "steps", "codecs", "cost", "latency"]
for w in which:
    globals()[w]()
