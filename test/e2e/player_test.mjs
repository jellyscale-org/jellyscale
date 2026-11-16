// The player helpers the e2e gates share (test/e2e/player.mjs: time, advances
// and seek, each taking the page first), played against a page whose <video>
// does what jellyfin-web's did in the run of JS-099 that failed with "Cannot
// read properties of null (reading 'seeking')": a viewer who switches from a
// burnt-in subtitle track to none is given a new source in the SAME element.
// The element is emptied (currentTime 0, nothing loaded), the player seeks it
// to the viewer's resume position, and then plays on from there. A seek that a
// test makes before that last step is overwritten by it, and at the film's end
// the player stops and takes the element out of the page.
//
// Time here is the number of reads: each page.evaluate is one tick, 0.2 s of
// media while the element plays, so the order of events never depends on how
// fast the machine runs the helpers (their 200 ms wait between polls only
// decides how long this test takes). Run: node test/e2e/player_test.mjs
// (test/scripts_test.sh does).
import assert from "node:assert/strict";
import test from "node:test";
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// The gates' helpers are in the one module that defines ffmpegRuns (test/scripts_test.sh checks that all seven of
// them are in one module); this test loads that module, whatever it is called. The names it must export are the
// ones a gate calls, and configure(options), which a gate calls once with { base, user, password, browser,
// failures, unanswered } before it makes a viewer.
const here = dirname(fileURLToPath(import.meta.url));
const definers = readdirSync(here).filter((file) => file.endsWith(".mjs") && !file.endsWith("_test.mjs") &&
  /^\s*(export\s+)?((async\s+)?function\s*\*?\s*|(const|let|var)\s+)ffmpegRuns\b/m.test(readFileSync(join(here, file), "utf8")));
assert.equal(definers.length, 1, `ffmpegRuns is defined in ${definers.length} modules: ${definers}`);
const { advances, configure, ffmpegRuns, seek, viewer } = await import(`./${definers[0]}`);

const RESUME = 102.4;
const FILM_END = 105;

// reload: the element was given its new source just before the first read.
// Without it the element has been playing at RESUME for a while.
function fakePage({ reload, gone = false }) {
  let tick = reload ? -1 : 9;
  let present = !gone;
  let now = reload ? 0 : RESUME;
  let readyState = reload ? 0 : 4;
  let seeking = false;
  let paused = reload;
  let seekingSince = -1;
  const video = {
    get currentTime() { return now; },
    set currentTime(t) {
      if (reload && tick < 3) return; // the player's own seek at tick 3 lands over it
      now = t;
      seeking = true;
      readyState = 1;
      seekingSince = tick;
    },
    get seeking() { return seeking; },
    get readyState() { return readyState; },
    get paused() { return paused; },
    pause() { paused = true; },
    play() { paused = false; },
  };
  // What the page does to the element between one read and the next.
  const step = () => {
    tick++;
    if (reload && tick === 1) [seeking, now, readyState] = [true, RESUME, 1];
    if (reload && tick === 2) [seeking, readyState] = [false, 4];
    if (reload && tick === 3) [now, paused, seeking, readyState] = [RESUME, false, false, 4];
    else if (seeking && seekingSince >= 0 && tick > seekingSince) [seeking, readyState] = [false, 4];
    else if (!paused && !seeking && readyState >= 3 && tick > 3) now += 0.2;
    if (now >= FILM_END) present = false;
  };
  return {
    video,
    async evaluate(fn, arg) {
      step();
      globalThis.document = { querySelector: (selector) => (selector === "video" && present ? video : null) };
      return fn(arg);
    },
  };
}

test("a seek after the subtitle change is not overwritten by the player's own seek to the resume position", async () => {
  const page = fakePage({ reload: true });
  await advances(page, 2);
  await seek(page, 60);
  assert.ok(page.video.currentTime > 60.5 && page.video.currentTime < 63, `playing at ${page.video.currentTime}`);
});

test("advances waits for the media to play on from where the player put it, not for the player's jump", async () => {
  const page = fakePage({ reload: true });
  await advances(page, 1.5);
  assert.ok(page.video.currentTime >= RESUME + 1.5, `returned at ${page.video.currentTime}, before the media played 1.5 s from ${RESUME}`);
});

test("a seek on a player that is playing lands and is read back playing", async () => {
  const page = fakePage({ reload: false });
  await seek(page, 60);
  assert.ok(page.video.currentTime > 60.5 && page.video.currentTime < 63, `playing at ${page.video.currentTime}`);
});

test("a player that has left the film still fails the seek", async () => {
  await assert.rejects(seek(fakePage({ reload: false, gone: true }), 60));
});

// --- viewer (JS-114): what a gate reads from a viewer's record means what it meant in its gate.
// A page that keeps its handlers, to fire events at; a browser that makes one context and page each time.
function recordingBrowser() {
  const pages = [];
  const newPage = async () => {
    const handlers = {};
    const page = {
      handlers,
      on(event, handler) { (handlers[event] ??= []).push(handler); },
      goto: async () => {}, waitForSelector: async () => {}, fill: async () => {}, click: async () => {},
      fire: async (event, ...args) => { for (const handler of handlers[event] ?? []) await handler(...args); },
    };
    pages.push(page);
    return page;
  };
  return { pages, newContext: async () => ({ newPage, close: async () => {} }) };
}
const base = "http://x";
const failedRequest = (path) => ({ url: () => base + path, method: () => "GET", failure: () => ({ errorText: "ERR_X" }), headers: () => ({}), postData: () => null });
const answered = (path, status, headers = {}) => ({
  url: () => base + path, status: () => status, headers: () => headers,
  request: () => ({ method: () => "GET", headers: () => ({}), postData: () => null }),
  json: async () => null,
});

async function viewerFor(options) {
  process.env.E2E_RESPONSES = `${tmpdir()}/player-test-responses-${process.pid}.jsonl`;
  const browser = recordingBrowser();
  configure({ base, user: "u", password: "p", browser, failures: [], ...options });
  const v = await viewer();
  return { ...v, page: browser.pages.at(-1), browser };
}

test("failover's viewer keeps every unanswered request as an object with a path; transcode's keeps HLS ones only", async () => {
  const all = await viewerFor({ unanswered: "all" });
  await all.page.fire("requestfailed", failedRequest("/Videos/1/hls1/main/3.mp4"));
  await all.page.fire("requestfailed", failedRequest("/Items"));
  assert.deepEqual(all.seen.failed.map((f) => f.path), ["/Videos/1/hls1/main/3.mp4", "/Items"]);
  const hls = await viewerFor({ unanswered: "hls" });
  await hls.page.fire("requestfailed", failedRequest("/Videos/1/hls1/main/3.mp4"));
  await hls.page.fire("requestfailed", failedRequest("/Items"));
  assert.deepEqual(hls.seen.failed, ["GET /Videos/1/hls1/main/3.mp4 ERR_X"]);
});

test("the subtitles gate keeps no unanswered request in seen.failed, whether HLS or not", async () => {
  const v = await viewerFor({ name: "subtitles", unanswered: undefined });
  await v.page.fire("requestfailed", failedRequest("/Videos/1/hls1/main/3.mp4"));
  await v.page.fire("requestfailed", failedRequest("/Items"));
  assert.deepEqual(v.seen.failed, []);
  configure({ name: undefined, unanswered: "none" }); // configure() persists; leave it as a gate with no name
});

test("a 5xx answer is recorded in errors, and a page error too", async () => {
  const v = await viewerFor({});
  await v.page.fire("response", answered("/Items", 500));
  await v.page.fire("pageerror", new Error("boom"));
  assert.deepEqual(v.seen.errors, ["500 /Items", "boom"]);
});

test("two viewers have contexts, pages and records of their own", async () => {
  const first = await viewerFor({});
  const second = await viewer();
  assert.notEqual(first.seen, second.seen);
  assert.notEqual(first.context, second.context);
  await first.page.fire("response", answered("/Items", 503));
  assert.equal(second.seen.errors.length, 0);
  assert.equal(first.browser.pages.length, 2);
});

test("a viewer made after the base changed signs in and records on the new one", async () => {
  await viewerFor({});
  configure({ base: "http://other" });
  const later = await viewer();
  await later.page.fire("response", answered("/Items", 500)); // the old base: not this viewer's server
  assert.equal(later.seen.errors.length, 0);
  await later.page.fire("response", { ...answered("/Items", 500), url: () => "http://other/Items" });
  assert.deepEqual(later.seen.errors, ["500 /Items"]);
  configure({ base });
});

test("every page a viewer opens records its responses for the conformance check", async () => {
  const v = await viewerFor({});
  const sinks = v.page.handlers.response.length;
  assert.ok(sinks >= 2, `only ${sinks} response handlers`);
});

// --- ffmpegRuns: the fields each gate reads, for the command lines the worker runs, through a pgrep that prints them.
function withPgrep(lines, run) {
  const dir = mkdtempSync(join(tmpdir(), "pgrep-"));
  const bin = join(dir, "bin");
  mkdirSync(bin);
  writeFileSync(join(dir, "lines"), lines.join("\n") + (lines.length ? "\n" : ""));
  writeFileSync(join(bin, "pgrep"), `#!/bin/sh\necho "$@" >"${dir}/args"\n[ -s "${dir}/lines" ] || exit 1\ncat "${dir}/lines"\n`);
  chmodSync(join(bin, "pgrep"), 0o755);
  const saved = [process.env.PATH, process.env.E2E_SCRATCH];
  process.env.PATH = `${bin}:${process.env.PATH}`;
  process.env.E2E_SCRATCH = "/scratch/e2e";
  try {
    return run(() => readFileSync(join(dir, "args"), "utf8").trim());
  } finally {
    [process.env.PATH, process.env.E2E_SCRATCH] = saved;
  }
}

test("ffmpegRuns says a re-encode by its -start_number, for subtitles and transcode alike", () => {
  const line = "4242 ffmpeg -nostdin -i /s/film.mkv -c:v libx264 -start_number 7 -map 0:1 out.m3u8";
  const [run, ...more] = withPgrep([line], (args) => {
    const runs = ffmpegRuns();
    assert.equal(args(), "-af ffmpeg.*/scratch/e2e"); // the worker's ffmpeg processes, by their scratch directory
    return runs;
  });
  assert.equal(more.length, 0);
  assert.equal(run.line, line);
  assert.deepEqual([run.copy, run.burns, run.startNumber, run.start, run.audio, run.inputs], [false, false, 7, 7, "1", 1]);
});

test("ffmpegRuns says a copy by -ss: transcode's start is its segment, subtitles' start_number is not a number", () => {
  const [run] = withPgrep(["17 ffmpeg -ss 30.5 -i /s/film.mkv -c:v copy -map 0:2 out.m3u8"], () => ffmpegRuns());
  assert.equal(run.copy, true);
  assert.ok(Number.isNaN(run.startNumber));
  assert.deepEqual([run.start, run.audio, run.inputs], [8, "2", 1]); // ceil(30.5 / 4)
});

test("ffmpegRuns says a burn-in by its filter, and two inputs by two -i", () => {
  const [burn, plain] = withPgrep([
    "1 ffmpeg -i a.mkv -i b.sub -filter_complex overlay=eof_action=pass -map 1:3 -start_number 2 o",
    "2 ffmpeg -i a.mkv -vf subtitles=subtitles.ass -start_number 0 o",
  ], () => ffmpegRuns());
  assert.deepEqual([burn.burns, burn.inputs, burn.audio, burn.startNumber], [true, 2, "3", 2]);
  assert.deepEqual([plain.burns, plain.inputs], [true, 1]);
  assert.equal(withPgrep(["3 ffmpeg -i a.mkv -start_number 0 o"], () => ffmpegRuns())[0].burns, false);
});

test("ffmpegRuns: a line with -ss and no -start_number has start NaN for subtitles, the segment for transcode and failover", () => {
  const line = "17 ffmpeg -ss 30.5 -i /s/film.mkv -c:v copy -map 0:2 out.m3u8";
  try {
    for (const [name, start] of [["subtitles", NaN], ["transcode", 8], ["failover", 8]]) {
      configure({ name });
      const [run] = withPgrep([line], () => ffmpegRuns());
      assert.ok(Object.is(run.start, start), `${name}: start ${run.start}`);
    }
  } finally {
    configure({ name: undefined });
  }
});

test("ffmpegRuns has one entry per ffmpeg process, and none when pgrep finds none", () => {
  const lines = ["10 ffmpeg -i a -start_number 0 o", "11 ffmpeg -i a -start_number 5 o", "12 ffmpeg -i a -ss 8 -c:v copy o"];
  assert.deepEqual(withPgrep(lines, () => ffmpegRuns()).map((run) => run.line), lines);
  assert.deepEqual(withPgrep([], () => ffmpegRuns()), []);
});

// --- what each gate reads from a viewer, by what the gate itself says.
const gateSource = (gate) => readFileSync(join(here, `${gate}.mjs`), "utf8");
const unansweredOf = (gate) => /configure\(\{[^}]*\bunanswered: "(all|hls)"/.exec(gateSource(gate))?.[1] ?? "hls";

test("failover's gate keeps every unanswered request as an object with a path; transcode's keeps HLS ones as text", async () => {
  assert.equal(unansweredOf("failover"), "all");
  assert.equal(unansweredOf("transcode"), "hls");
  for (const gate of ["failover", "transcode"]) {
    const v = await viewerFor({ unanswered: unansweredOf(gate) });
    await v.page.fire("requestfailed", failedRequest("/Videos/1/hls1/main/3.mp4"));
    await v.page.fire("requestfailed", failedRequest("/Items"));
    assert.deepEqual(v.seen.failed.map((f) => f.path ?? f), gate === "failover" ? ["/Videos/1/hls1/main/3.mp4", "/Items"] : ["GET /Videos/1/hls1/main/3.mp4 ERR_X"], gate);
  }
});

test("a 5xx answer fails noErrors in the subtitles and transcode gates, and a 4xx answer on a page does not", async () => {
  for (const gate of ["subtitles", "transcode"]) {
    const found = /^function noErrors\(seen\) \{\n[\s\S]*?\n\}\n/m.exec(gateSource(gate));
    assert.ok(found, `${gate}.mjs has no noErrors`);
    const noErrors = new Function(`${found[0]}; return noErrors;`)();
    const quiet = await viewerFor({ unanswered: unansweredOf(gate) });
    noErrors(quiet.seen);
    await quiet.page.fire("response", answered("/Items", 404));
    noErrors(quiet.seen);
    const broken = await viewerFor({ unanswered: unansweredOf(gate) });
    await broken.page.fire("response", answered("/Items", 500));
    assert.throws(() => noErrors(broken.seen), /500 \/Items/, gate);
  }
});
