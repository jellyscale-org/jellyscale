// The helpers the e2e gates share. The player's take the page first. A gate calls configure() once, with the
// browser it launched, the server it tests, the sign-in and where it keeps its failures; viewer, login, scenario
// and the rest then work for that gate.
import { execFileSync } from "node:child_process";
import { recordResponses } from "./conform.mjs";
import { schedule } from "./shard.mjs";

const gate = { reasonLines: 1, ffmpegWait: 60000, unanswered: "none" };

// name: the gate's, for its screenshot. base, user, password: the server and the account. browser: the launched one.
// failures: the gate's list, which scenario() adds to. reasonLines: how many lines of an error a failure says.
// ffmpegWait: how long a scenario may leave ffmpeg running. afterScenario: what a gate does after each scenario.
// unanswered: which requests that ended without an answer seen.failed holds, "hls" (as strings), "all" (as
// objects) or "none" (the default: subtitles never kept any).
export function configure(options) {
  Object.assign(gate, options);
}

export const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);

export async function waitFor(what, test, timeout = 30000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

// Returns once the media has played `seconds` on from where it settled. A player
// that is given a new source (a viewer changing subtitle track) empties the
// element and then jumps it to the resume position; that jump is not playback,
// and a seek made before the player's own start seek is overwritten by it (JS-099).
export async function advances(page, seconds = 1.5) {
  let from = null;
  await waitFor(`currentTime to play ${seconds} s on`, async () => {
    const { t, playing } = await page.evaluate(() => {
      const video = document.querySelector("video");
      if (!video) return { t: -1, playing: false };
      return { t: video.currentTime, playing: !video.seeking && video.readyState >= 3 && !video.paused };
    });
    if (!playing) return false;
    from ??= t;
    return t > from + seconds;
  }, 40000);
}

// Seeks, and returns once the picture is playing a little past the target, so a
// frame read from it is one the player has really shown.
export async function seek(page, seconds) {
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, seconds);
  await waitFor(`playback past ${seconds}`, async () => page.evaluate((t) => {
    const video = document.querySelector("video");
    return !video.seeking && video.readyState >= 3 && video.currentTime > t + 0.5 && video.currentTime < t + 3;
  }, seconds), 60000);
}

export const sql = (text) => execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();

// One ffmpeg command line as the fields the gates read. A re-encode says its first segment in -start_number; a
// copy says where it seeks to, the keyframe before its first segment, from which the segment follows on these
// films (a keyframe every two seconds, segments of four). startNumber is NaN for a line without -start_number.
// seeks: whether start falls back to the -ss seek; the subtitles gate read -start_number alone, NaN without it.
export function parseFfmpeg(line, seeks = true) {
  const number = /-start_number (\d+)/.exec(line)?.[1];
  return {
    line,
    pid: Number(line.split(" ")[0]),
    copy: /-c:v copy/.test(line),
    burns: /subtitles=subtitles\.ass|overlay=eof_action/.test(line),
    startNumber: Number(number),
    start: number !== undefined ? Number(number) : !seeks ? NaN : Math.ceil(Number(/-ss ([\d.]+)/.exec(line)?.[1] ?? 0) / 4),
    // The audio track's map: from the second input, or from the only one.
    audio: /-map [01]:(\d+)/.exec(line)?.[1],
    inputs: (line.match(/ -i /g) ?? []).length,
  };
}

// The worker's ffmpeg processes, one entry each, read from the process table.
export function ffmpegRuns() {
  try {
    return execFileSync("pgrep", ["-af", `ffmpeg.*${process.env.E2E_SCRATCH}`]).toString().trim().split("\n").filter(Boolean).map((line) => parseFfmpeg(line, gate.name !== "subtitles"));
  } catch {
    return []; // pgrep exits 1 when nothing matches.
  }
}

// How many processes the worker has as children. A zombie child still counts: the listing is by parent alone.
export function workerChildren() {
  try {
    return execFileSync("pgrep", ["-P", process.env.E2E_WORKER_PID]).toString().trim().split("\n").filter(Boolean).length;
  } catch {
    return 0; // pgrep exits 1 when nothing matches.
  }
}

export async function login(page) {
  await page.goto(`${gate.base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", gate.user);
  await page.fill("#txtManualPassword", gate.password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
}

// Whether the page shows the sign-in form: a viewer whose session ended is sent back to it.
export const signInShown = (page) => page.locator("#txtManualName").isVisible().catch(() => false);

function clock() { return Date.now(); }

// A signed-in browser in a context of its own, which records what the server told it and the page did. The gates
// read different parts of what it keeps; each part is kept for every gate.
export async function viewer() {
  const { base } = gate;
  const context = await gate.browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  recordResponses(page, base);
  const seen = {
    infos: [], hls: [], playlists: [], subtitles: [], attachments: [], configs: 0, deletes: [], errors: [], console: [],
    failed: [], cut: [], requests: [], answers: [], reports: [], sockets: [], open: new Set(),
  };
  const ours = (url) => url.startsWith(base);
  // The player's playback requests, for a failure to show what it did and when.
  const began = Date.now();
  page.timeline = [];
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("console", (message) => { if (/hls|playback/i.test(message.text())) seen.console.push(message.text().slice(0, 200)); });
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (ours(request.url())) { seen.requests.push({ at: Date.now(), method: request.method(), url: request.url(), range: request.headers().range }); seen.open.add(request); }
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
    if (request.method() === "POST" && /^\/Sessions\/Playing\/(Progress|Stopped)$/i.test(url.pathname)) {
      seen.reports.push({ at: Date.now(), kind: /Stopped/i.test(url.pathname) ? "Stopped" : "Progress", request, body: JSON.parse(request.postData() ?? "{}"), status: null });
    }
    if (/PlaybackInfo|Sessions\/Playing|ActiveEncodings|master\.m3u8|main\.m3u8/i.test(url.pathname)) {
      page.timeline.push(`${Date.now() - began} ms ${request.method()} ${url.pathname.replace(/^.*\/(?=[^/]+\/?[^/]*$)/, "")} ${url.searchParams.get("PlaySessionId") ?? ""}`);
    }
  });
  page.on("requestfinished", (request) => seen.open.delete(request));
  // A request that ended without an answer (a response cut, a connection refused, hls.js giving up after ten
  // seconds). A seek aborts some too, so a scenario that seeks looks before it does.
  page.on("requestfailed", (request) => {
    seen.open.delete(request);
    if (!ours(request.url())) return;
    const path = new URL(request.url()).pathname;
    seen.cut.push({ at: Date.now(), method: request.method(), url: request.url(), error: request.failure()?.errorText });
    if (gate.unanswered === "all") seen.failed.push({ at: Date.now(), method: request.method(), path, error: request.failure()?.errorText });
    else if (gate.unanswered === "hls" && /\/hls1\//i.test(request.url())) seen.failed.push(`${request.method()} ${path} ${request.failure()?.errorText}`);
    const report = seen.reports.find((r) => r.request === request);
    if (report) report.status = 0;
  });
  page.on("response", async (response) => {
    if (!ours(response.url())) return;
    const url = new URL(response.url());
    const method = response.request().method();
    const answer = {
      at: Date.now(), status: response.status(), method, path: url.pathname, url: response.url(), range: response.request().headers().range,
      routed: response.headers()["x-routed-to"], backend: Number(response.headers()["x-backend"]) || null,
    };
    seen.answers.push(answer);
    const report = seen.reports.find((r) => r.request === response.request());
    if (report) Object.assign(report, { status: answer.status, backend: answer.backend });
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/\.m3u8$/i.test(url.pathname)) seen.playlists.push(url.pathname + url.search);
    if (/^\/videos\/.*(m3u8|\/hls1\/)/i.test(url.pathname)) {
      seen.hls.push({ at: Date.now(), status: response.status(), routed: answer.routed, path: url.pathname, query: url.searchParams });
    }
    if (/\/Subtitles\//i.test(url.pathname)) {
      seen.subtitles.push({ at: Date.now(), status: response.status(), type: response.headers()["content-type"], path: url.pathname, index: Number(url.pathname.split("/")[5]) });
    }
    if (/\/Attachments\/\d+$/i.test(url.pathname)) {
      seen.attachments.push({ status: response.status(), type: response.headers()["content-type"], path: url.pathname, bytes: (await response.body().catch(() => Buffer.alloc(0))).length });
    }
    if (/\/System\/Configuration\/encoding$/i.test(url.pathname) && response.status() === 200) seen.configs++;
    if (method === "POST" && /\/PlaybackInfo$/i.test(url.pathname)) {
      const request = JSON.parse(response.request().postData() ?? "{}");
      const reply = await response.json().catch(() => null);
      if (reply) seen.infos.push({ request, source: reply.MediaSources[0], session: reply.PlaySessionId });
    }
  });
  page.on("websocket", (ws) => {
    const socket = { url: ws.url(), openedAt: Date.now(), closedAt: null, frames: [], token: new URL(ws.url()).searchParams.get("ApiKey") ?? new URL(ws.url()).searchParams.get("api_key") };
    ws.on("framereceived", ({ payload }) => socket.frames.push(String(payload).slice(0, 200)));
    ws.on("close", () => { socket.closedAt = clock(); });
    seen.sockets.push(socket);
  });
  await login(page);
  return { context, page, seen };
}

// A scenario of the gate, on a viewer of its own. A failure, or ffmpeg left running, names the scenario in the
// gate's failures and the next scenario runs. E2E_ONLY picks scenarios by name and E2E_SHARD by call order
// (shard.mjs); a name is a quoted literal, with any part known only at run time after ": ", so that
// test/report.sh can annotate the scenario at its own line.
export function scenario(name, run, options = {}) {
  return schedule(name, options, async () => {
    const began = Date.now();
    const v = await viewer();
    let passed = false;
    try {
      await run(v);
      passed = true;
    } catch (error) {
      const why = error.message.split("\n").filter(Boolean).slice(0, gate.reasonLines).join(" / ");
      gate.failures.push(`${name}: ${why}`);
      console.log(`FAIL  ${name}: ${why} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-${gate.name}.png` }).catch(() => {});
    } finally {
      await v.context.close().catch(() => {});
      await gate.afterScenario?.();
    }
    // Nothing may be left running for the next scenario.
    await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, gate.ffmpegWait).catch(() => {
      gate.failures.push(`${name}: ffmpeg still running afterwards`);
      console.log(`FAIL  ${name}: ffmpeg still running afterwards (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      passed = false;
    });
    if (passed) console.log(`ok    ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  });
}
