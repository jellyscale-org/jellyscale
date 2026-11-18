// What the e2e gates need to watch a film that fails under the player (JS-088): the router's faults, the position
// of the picture sampled while it plays, and the counters of the stack's processes.

// Sets the router's rules (see router.mjs); {} reads them.
export async function routerFaults(base, rules) {
  const response = await fetch(`${base}/__router`, { method: "PUT", body: JSON.stringify(rules ? { faults: rules } : {}) });
  return (await response.json()).faults;
}

// Starts sampling the element every 100 ms, in the page, so that what the picture did between two reads of the
// test is kept. A page that replaces the element is read through its new one.
export function startSampler(page) {
  return page.evaluate(() => {
    window.__samples = [];
    clearInterval(window.__sampler);
    window.__sampler = setInterval(() => {
      const video = document.querySelector("video");
      if (video) window.__samples.push({ t: video.currentTime, w: performance.now(), seeking: video.seeking, paused: video.paused });
    }, 100);
  });
}

// What the decoder says of the picture. A bad picture shows here and not in currentTime: with a re-encode under an
// init it cannot decode, Chrome (jellyfin-web, hls.js 1.6.16) sets video.error to PIPELINE_ERROR_DECODE and stops
// counting frames, while corruptedVideoFrames stays 0 (the spike is in compat/captures/segment-errors-12.4.md), so the
// frames that were shown and the error are what a scenario asserts.
export const quality = (page) => page.evaluate(() => {
  const video = document.querySelector("video");
  const q = video.getVideoPlaybackQuality();
  return { error: video.error ? video.error.message || `code ${video.error.code}` : null, total: q.totalVideoFrames, corrupted: q.corruptedVideoFrames };
});

export const samples = (page) => page.evaluate(() => window.__samples ?? []);

// What the viewer saw of the position, from `from` on: it never goes back by more than `back` seconds from the
// furthest it reached (a segment, 4 s) and never jumps forward by more than `forward` seconds beyond what the
// clock allowed between two samples. Samples before the first one at or past `target` are the seek's own jump.
// Returns the problems, in words.
export function continuity(all, target, { back = 4, forward = 1 } = {}) {
  const first = all.findIndex((s) => s.t >= target - 0.01);
  if (first < 0) return [`the position never reached ${target} s`];
  const found = [];
  let peak = all[first].t;
  for (let i = first + 1; i < all.length; i++) {
    const [a, b] = [all[i - 1], all[i]];
    if (b.t < peak - back) found.push(`went back to ${b.t.toFixed(1)} s from ${peak.toFixed(1)} s`);
    const allowed = (b.w - a.w) / 1000 + forward;
    if (b.t - a.t > allowed) found.push(`skipped forward from ${a.t.toFixed(1)} s to ${b.t.toFixed(1)} s in ${((b.w - a.w) / 1000).toFixed(1)} s`);
    peak = Math.max(peak, b.t);
  }
  return found;
}

// The sum, over the processes of the stack that serve /metrics (E2E_METRICS, a list of ports), of the counters
// whose name has "fallback" in it and ends in _total: how many streams have fallen back from a copy to a
// re-encode. More than one such name is a mistake of the build's, which the scenario says.
export async function fallbacks() {
  const ports = (process.env.E2E_METRICS ?? "").split(",").filter(Boolean);
  if (!ports.length) throw new Error("E2E_METRICS lists the ports of the stack's /metrics (test/e2e/run.sh sets it)");
  const names = new Set();
  let sum = 0;
  for (const port of ports) {
    const text = await (await fetch(`http://127.0.0.1:${port}/metrics`)).text();
    for (const line of text.split("\n")) {
      const match = /^(jellyscale_[a-z_]*fallback[a-z_]*_total)(\{[^}]*\})? ([0-9.e+]+)$/.exec(line);
      if (match) { names.add(match[1]); sum += Number(match[3]); }
    }
  }
  if (names.size > 1) throw new Error(`more than one fallback counter: ${[...names]}`);
  return { sum, name: [...names][0] ?? null };
}
