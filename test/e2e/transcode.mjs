// Phase 3 gate: unmodified jellyfin-web plays films Chrome can't direct-play,
// through the router with two api replicas, a stream replica and a worker on
// its own process, and the transcode ends when it should.
// Usage: E2E_SCRATCH=<worker scratch dir> node transcode.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFileSync, spawn } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [home, user, password] = process.argv.slice(2);
let base = home;
const scratch = process.env.E2E_SCRATCH;
if (!scratch) throw new Error("E2E_SCRATCH is the worker's scratch directory");

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const failures = [];

function sql(text) {
  return execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();
}

// The worker's ffmpeg processes, read from the process table.
function ffmpegRuns() {
  try {
    const lines = execFileSync("pgrep", ["-af", `ffmpeg.*${scratch}`]).toString().trim().split("\n").filter(Boolean);
    return lines.map((line) => ({ line, start: Number(/-start_number (\d+)/.exec(line)?.[1]), audio: /-map 1:(\d+)/.exec(line)?.[1] }));
  } catch {
    return []; // pgrep exits 1 when nothing matches.
  }
}

async function login(page) {
  await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", user);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
}

// A signed-in browser that records what the server told it.
async function viewer() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  const seen = { infos: [], hls: [], deletes: [], errors: [], console: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("console", (message) => { if (/hls|playback/i.test(message.text())) seen.console.push(message.text().slice(0, 200)); });
  page.on("request", (request) => {
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
  });
  page.on("response", async (response) => {
    const url = new URL(response.url());
    if (!response.url().startsWith(base)) return;
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/^\/videos\/.*(m3u8|\/hls1\/)/i.test(url.pathname)) {
      seen.hls.push({ at: Date.now(), status: response.status(), routed: response.headers()["x-routed-to"], path: url.pathname, query: url.searchParams });
    }
    if (response.request().method() === "POST" && /\/PlaybackInfo$/i.test(url.pathname)) {
      const request = JSON.parse(response.request().postData() ?? "{}");
      const reply = await response.json().catch(() => null);
      if (reply) seen.infos.push({ request, source: reply.MediaSources[0], session: reply.PlaySessionId });
    }
  });
  await login(page);
  return { context, page, seen };
}

const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);
const duration = (page) => page.evaluate(() => document.querySelector("video")?.duration ?? 0);

async function waitFor(what, test, timeout = 30000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function play(page, library, title) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
}

async function advances(page, seconds = 1.5) {
  const before = await time(page);
  await waitFor(`currentTime to pass ${before + seconds}`, async () => (await time(page)) > before + seconds, 40000);
}

function noErrors(seen) {
  if (seen.errors.length) throw new Error(`errors: ${seen.errors.join("; ")}`);
  const bad = seen.hls.filter((r) => r.status >= 400 && r.status !== 410);
  if (bad.length) throw new Error(`HLS requests failed: ${JSON.stringify(bad.slice(0, 3))}`);
}

const transcodeUrl = (seen) => seen.infos.map((i) => i.source.TranscodingUrl).filter(Boolean).at(-1);

// E2E_ONLY="audio" runs just the scenarios whose name matches.
async function scenario(name, run) {
  if (process.env.E2E_ONLY && !new RegExp(process.env.E2E_ONLY, "i").test(name)) return;
  const v = await viewer();
  try {
    await run(v);
    console.log(`ok    ${name}`);
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name}: ${error.message.split("\n")[0]}`);
    await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-transcode.png` }).catch(() => {});
  } finally {
    await v.context.close().catch(() => {});
  }
  // Nothing may be left running for the next scenario.
  await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 60000).catch(() => failures.push(`${name}: ffmpeg still running afterwards`));
}

const leave = async (page) => {
  const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 });
  await page.evaluate(() => window.history.back());
  const response = await stopped;
  if (response.status() !== 204) throw new Error(`Stopped answered ${response.status()}`);
  await response.finished();
  const timing = response.request().timing();
  console.log(`      Stopped answered in ${Math.round(timing.responseEnd - timing.requestStart)} ms`);
};

await scenario("POSIX: an AC3 film transcodes, plays from the stream replica, then far seek and stop end the right ffmpeg", async ({ page, seen }) => {
  await play(page, "Transcode", "Zeta Film");
  const source = seen.infos.at(-1).source;
  if (source.SupportsDirectPlay !== false || source.TranscodingSubProtocol !== "hls" || source.TranscodingContainer !== "mp4") {
    throw new Error(`PlaybackInfo did not ask for a transcode: ${JSON.stringify(source).slice(0, 300)}`);
  }
  if (!/TranscodeReasons=AudioCodecNotSupported/.test(source.TranscodingUrl)) throw new Error(`reasons: ${source.TranscodingUrl}`);
  if (!(await page.evaluate(() => document.querySelector("video").src.startsWith("blob:")))) throw new Error("not played through MSE");
  await advances(page);
  noErrors(seen);
  if (!seen.hls.some((r) => /master\.m3u8$/i.test(r.path)) || !seen.hls.some((r) => /\/hls1\/main\/0\.mp4$/i.test(r.path))) throw new Error("no master or segment 0");
  if (seen.hls.some((r) => r.routed !== "stream")) throw new Error("HLS was not served by the stream replica");

  // Far past the encoded point: a job starts there, and the first one stops.
  const first = ffmpegRuns();
  if (first.length !== 1) throw new Error(`expected one ffmpeg, saw ${first.length}`);
  const total = await duration(page);
  const target = total * 0.8;
  const seeked = Date.now();
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  const far = await waitFor("the first segment of the far seek", async () => seen.hls.find((r) => r.at > seeked && r.status === 200 && Number(r.path.match(/\/(\d+)\.mp4$/i)?.[1]) >= Math.floor(target / 4) - 1), 60000);
  console.log(`      far seek: first segment answered ${far.at - seeked} ms after the seek`);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
  await waitFor("the replaced ffmpeg to exit and one to start at the seek", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].start >= Math.floor(target / 4) - 2;
  }, 20000).catch(async () => { throw new Error(`ffmpeg runs after the seek: ${JSON.stringify(ffmpegRuns().map((r) => r.start))}`); });
  noErrors(seen);

  await leave(page);
  await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 20000);
});

await scenario("S3: a film from the bucket transcodes and plays", async ({ page, seen }) => {
  await play(page, "Cloud", "Eta Film");
  if (seen.infos.at(-1).source.TranscodingUrl === undefined) throw new Error("no TranscodingUrl");
  await advances(page, 3);
  noErrors(seen);
  if (!seen.hls.some((r) => /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200)) throw new Error("no segment was served");
  await leave(page);
});

await scenario("the quality menu cap transcodes a film that direct plays", async ({ page, seen }) => {
  await play(page, "Movies", "Alpha Film");
  if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("Alpha should direct play at Auto");
  await page.mouse.move(600, 400);
  await page.click(".btnVideoOsdSettings");
  await page.click('[data-id="quality"]');
  await page.click('[data-id="420000"]');
  const info = await waitFor("a PlaybackInfo with the cap", async () => seen.infos.find((i) => i.request.MaxStreamingBitrate === 420000));
  const query = new URL(info.source.TranscodingUrl, base).searchParams;
  if (!/ContainerBitrateExceedsLimit/.test(query.get("TranscodeReasons") ?? "")) throw new Error(`reasons: ${query.get("TranscodeReasons")}`);
  if (Number(query.get("VideoBitrate")) + Number(query.get("AudioBitrate")) !== 420000) throw new Error(`split: ${query.get("VideoBitrate")} + ${query.get("AudioBitrate")}`);
  await waitFor("the transcode to play", async () => seen.hls.some((r) => /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page);
  noErrors(seen);
  await leave(page);
});

await scenario("switching audio plays the new track and the old job stops", async ({ page, seen }) => {
  await play(page, "Transcode", "Theta Film");
  await advances(page);
  const before = ffmpegRuns();
  if (before.length !== 1 || before[0].audio !== "1") throw new Error(`expected one ffmpeg on track 1: ${JSON.stringify(before)}`);
  const oldSession = seen.infos.at(-1).session;
  await page.mouse.move(600, 400);
  await page.click(".btnAudio");
  await page.click('[data-id="2"]');
  const info = await waitFor("a PlaybackInfo for track 2", async () => seen.infos.find((i) => Number(i.request.AudioStreamIndex) === 2));
  if (info.session === oldSession) throw new Error("the session id did not change");
  await waitFor("DELETE ActiveEncodings for the old session", async () => seen.deletes.some((u) => u.includes(oldSession)));
  await waitFor("segments of the new stream", async () => seen.hls.some((r) => r.query.get("AudioStreamIndex") === "2" && /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page);
  await waitFor("only the new track's ffmpeg to be left", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].audio === "2";
  }, 20000).catch(() => { throw new Error(`ffmpeg runs after the switch: ${JSON.stringify(ffmpegRuns())}`); });
  noErrors(seen);
  await leave(page);
});

await scenario("closing the tab ends ffmpeg once the idle time passes", async ({ context, page, seen }) => {
  await play(page, "Transcode", "Iota Film");
  await advances(page);
  if (ffmpegRuns().length !== 1) throw new Error("no ffmpeg to end");
  await page.close(); // Sends no Stopped and no DELETE.
  await new Promise((resolve) => setTimeout(resolve, 2000));
  if (ffmpegRuns().length !== 1) throw new Error("ffmpeg ended before the idle time, so this proves nothing");
  await waitFor("ffmpeg to end", async () => ffmpegRuns().length === 0, 45000);
});

await scenario("DELETE ActiveEncodings ends ffmpeg", async ({ page, seen }) => {
  await play(page, "Transcode", "Kappa Film");
  await advances(page);
  if (ffmpegRuns().length !== 1) throw new Error("no ffmpeg to end");
  const session = seen.infos.at(-1).session;
  const status = await page.evaluate(async (id) => {
    const client = window.ApiClient;
    // Twice, as jellyfin-web sends it.
    const calls = [0, 1].map(() => fetch(`${client.serverAddress()}/Videos/ActiveEncodings?deviceId=${encodeURIComponent(client.deviceId())}&PlaySessionId=${id}`,
      { method: "DELETE", headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"` } }).then((r) => r.status));
    return Promise.all(calls);
  }, session);
  if (status.join() !== "204,204") throw new Error(`DELETE answered ${status}`);
  await waitFor("ffmpeg to end", async () => ffmpegRuns().length === 0, 20000);
});

// Two viewers on one spec share a job, and stopping one leaves the other.
if (!process.env.E2E_ONLY) {
  const a = await viewer();
  const b = await viewer();
  try {
    await play(a.page, "Transcode", "Lambda Film");
    await play(b.page, "Transcode", "Lambda Film");
    await advances(a.page, 1);
    await advances(b.page, 1);
    if (ffmpegRuns().length < 1) throw new Error("no ffmpeg is running for two viewers");
    await leave(a.page);
    // Several heartbeats pass.
    await new Promise((resolve) => setTimeout(resolve, 6000));
    await advances(b.page, 2);
    if (ffmpegRuns().length < 1) throw new Error("stopping one viewer ended the other's ffmpeg");
    noErrors(b.seen);
    await leave(b.page);
    await waitFor("ffmpeg to end once both left", async () => ffmpegRuns().length === 0, 20000);
    console.log("ok    two viewers share a job and stopping one leaves the other playing");
  } catch (error) {
    failures.push(`two viewers: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  two viewers: ${error.message.split("\n")[0]}`);
  } finally {
    await a.context.close().catch(() => {});
    await b.context.close().catch(() => {});
  }
}

// Reports a position as a player leaving would, then opens the film from its
// details page and presses Resume. Returns how long the first frame took.
async function resume(page, library, title, seconds) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().waitFor();
  const id = new URLSearchParams(page.url().split("?")[1]).get("id");
  await page.evaluate(async ([id, ticks]) => {
    const client = window.ApiClient;
    await fetch(`${client.serverAddress()}/Sessions/Playing/Stopped`, {
      method: "POST",
      headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"`, "Content-Type": "application/json" },
      body: JSON.stringify({ ItemId: id, PositionTicks: ticks }),
    });
  }, [id, seconds * 10_000_000]);
  await page.reload();
  const button = page.locator(".btnPlay[data-action=resume]:visible").first();
  await button.waitFor({ timeout: 30000 });
  const started = Date.now();
  await button.click();
  // Every ffmpeg start number seen while it loads.
  const starts = new Set();
  await waitFor("the film to play from the resume point", async () => {
    for (const run of ffmpegRuns()) starts.add(run.start);
    return (await time(page)) > seconds + 0.1;
  }, 60000);
  return { firstFrameMs: Date.now() - started, starts: [...starts] };
}

await scenario("resuming mid-film starts one job, at the resume point", async ({ page, seen }) => {
  const { firstFrameMs, starts } = await resume(page, "Transcode", "Mu Film", 40);
  console.log(`      time to first frame on resume: ${firstFrameMs} ms; ffmpeg start segments seen: ${starts}`);
  if (starts.length !== 1 || starts[0] !== 10) throw new Error(`ffmpeg started at ${starts}, want only segment 10`);
  noErrors(seen);
  await leave(page);
});

// A transcode that breaks must end in the dialog, not in endless asking.
await scenario("a transcode that fails: the viewer gets the dialog, and the client stops asking", async ({ page, seen }) => {
  const media = process.env.E2E_MEDIA;
  if (!media) throw new Error("E2E_MEDIA is the transcode library directory");
  const file = `${media}/Xi Film (2031)/Xi Film (2031).mkv`;
  // The scanner's probe is in the database; the file the worker reads is not media.
  writeFileSync(file, readFileSync(file).subarray(0, 4096));
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Xi Film" }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await page.getByText("Playback Error").first().waitFor({ timeout: 60000 });
  await new Promise((resolve) => setTimeout(resolve, 8000));
  if (seen.infos.length > 4) throw new Error(`the client asked ${seen.infos.length} times`);
  if (seen.hls.some((r) => r.status === 200 && /\/hls1\/main\/\d+\.mp4$/i.test(r.path))) throw new Error("a broken file produced a segment");
});

// A worker whose transcodeStore differs from the replicas': refused, with the
// cause in the log. The row is a worker that stays alive.
await scenario("a worker with another transcodeStore: the dialog, and the log names the cause", async ({ page, seen }) => {
  if (!process.env.E2E_PG) throw new Error("E2E_PG is the Postgres container");
  sql("INSERT INTO transcode_nodes (id, roles, slots, store, seen_at) VALUES ('elsewhere', 'worker', 2, 'file:///other/segments', now() + interval '1 hour')");
  try {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 20000 });
    await new Promise((resolve) => setTimeout(resolve, 5000));
    if (seen.infos.length > 2) throw new Error(`the client asked ${seen.infos.length} times`);
    if (seen.hls.length) throw new Error("the client got a stream it could not play");
    const log = readFileSync(process.env.E2E_LOG, "utf8");
    if (!log.includes("file:///other/segments") || !log.includes("must be the same on every role")) throw new Error("the log doesn't name the worker's store");
  } finally {
    sql("DELETE FROM transcode_nodes WHERE id = 'elsewhere'");
  }
});

// ffmpeg that hangs, with the worker still up and renewing the lease. The
// browser abandons a request after about ten seconds and sends it again, so
// the server's own deadline never reaches it; the worker has to fail the job.
await scenario("ffmpeg hangs: the viewer gets the dialog, and the client stops asking", async ({ page, seen }) => {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Omicron Film" }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("ffmpeg to start", async () => ffmpegRuns().length === 1, 20000);
  for (const pid of execFileSync("pgrep", ["-f", `ffmpeg.*${scratch}`]).toString().trim().split("\n")) process.kill(Number(pid), "SIGSTOP");
  await page.getByText("Playback Error").first().waitFor({ timeout: 90000 });
  await new Promise((resolve) => setTimeout(resolve, 8000));
  if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
  if (!readFileSync(process.env.E2E_LOG, "utf8").includes("ffmpeg made no segment")) throw new Error("the log doesn't say ffmpeg stalled");
});

// A stream replica with no transcodeStore registers that, and the api replicas
// find the difference among the roles, so the viewer gets the dialog and not a
// loop.
await scenario("a stream replica without transcodeStore: the dialog, and the log names the cause", async ({ page, seen }) => {
  const nostore = process.env.E2E_BASE_NOSTORE;
  if (!nostore || !process.env.E2E_NOSTORE) throw new Error("E2E_NOSTORE starts a stream replica without a transcodeStore, behind E2E_BASE_NOSTORE");
  const replica = spawn(process.env.E2E_NOSTORE, [], { stdio: "ignore" });
  base = nostore;
  try {
    await waitFor("the replica to register", async () => sql("SELECT count(*) FROM transcode_nodes WHERE roles = 'stream' AND store = ''") === "1", 30000);
    // A new login, as the viewer was made against the other router.
    await login(page);
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 30000 });
    await new Promise((resolve) => setTimeout(resolve, 8000));
    if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
    if (!readFileSync(process.env.E2E_LOG, "utf8").includes("stream has no transcodeStore")) throw new Error("the log doesn't name the missing store");
  } finally {
    base = home;
    const gone = new Promise((resolve) => replica.once("exit", resolve));
    replica.kill("SIGTERM");
    await gone;
  }
});

// Last, because it stops the worker. A viewer whose transcode can't start must
// be told, not left to jellyfin-web's retries, which never end on their own.
const workerPid = Number(process.env.E2E_WORKER_PID);
if (workerPid && (!process.env.E2E_ONLY || /no worker/i.test(process.env.E2E_ONLY))) {
  // The worker goes away while a film plays. The player abandons a request
  // after ten seconds, so a server that answers later leaves it asking for
  // ever; it must answer sooner. jellyfin-web shows no dialog for an error
  // after playback began: it drops the stream, so the picture stops and the
  // client asks for nothing more. The log has to say why.
  await scenario("worker lost mid-film: the player gets an answer in time and stops asking", async ({ page, seen }) => {
    await play(page, "Transcode", "Pi Film");
    await advances(page);
    process.kill(workerPid, "SIGKILL");
    const target = (await duration(page)) * 0.8;
    const seeked = Date.now();
    await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
    const answer = await waitFor("an error answer to the segment", async () => seen.hls.find((r) => r.status >= 500 && /\/hls1\/main\/\d+\.mp4$/i.test(r.path)), 30000);
    console.log(`      worker killed, far seek: the segment request was answered ${answer.status} after ${answer.at - seeked} ms`);
    if (answer.at - seeked > 9000) throw new Error(`the answer took ${answer.at - seeked} ms, and the player gives up at 10000`);
    const asked = [seen.hls.length, seen.infos.length];
    await new Promise((resolve) => setTimeout(resolve, 12000));
    if (seen.hls.length !== asked[0] || seen.infos.length !== asked[1]) throw new Error("the client kept asking after the refusal");
    // The dead worker still counts as a worker for 10 seconds, so the log may say its slots were busy.
    if (!/a transcode job found (no worker|every slot busy)/.test(readFileSync(process.env.E2E_LOG, "utf8"))) throw new Error("the log doesn't say why the job found no worker");
  });

  await scenario("no worker: the viewer gets a playback error dialog, and the client stops asking", async ({ page, seen }) => {
    try { process.kill(workerPid, "SIGTERM"); } catch {} // Already gone after the scenario above.
    await new Promise((resolve) => setTimeout(resolve, 11000)); // The server forgets a worker after 10 s.
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 20000 });
    await new Promise((resolve) => setTimeout(resolve, 5000));
    if (seen.infos.length > 2) throw new Error(`the client asked ${seen.infos.length} times`);
    if (seen.hls.length) throw new Error("the client got a stream it could not play");
  });
}

// Still without a worker: a film that direct plays keeps playing when the
// viewer picks its other audio track, which would have needed a transcode.
if (workerPid && (!process.env.E2E_ONLY || /no worker/i.test(process.env.E2E_ONLY))) {
  await scenario("no worker: switching audio on a direct play film keeps it playing", async ({ page, seen }) => {
    await play(page, "Transcode", "Nu Film");
    if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("Nu Film should direct play");
    await advances(page);
    await page.mouse.move(600, 400);
    await page.click(".btnAudio");
    await page.click('[data-id="2"]');
    await waitFor("a PlaybackInfo for track 2", async () => seen.infos.find((i) => Number(i.request.AudioStreamIndex) === 2));
    if (await page.getByText("Playback Error").count()) throw new Error("the viewer got an error dialog");
    await new Promise((resolve) => setTimeout(resolve, 3000));
    await advances(page, 2);
    if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
    await leave(page);
  });
}

await browser.close();
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log("\nPASS");
