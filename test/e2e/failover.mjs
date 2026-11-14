// JS-031 gate: replicas die under a playing film and the viewer does not notice. Each
// scenario starts the api or stream replicas it needs on ports of its own, points the router
// at them (test/e2e/router.mjs, PUT /__router), plays a film in unmodified jellyfin-web, and
// then kills a replica with SIGKILL, or stops it with SIGTERM and starts a replacement. The
// router polls GET /readyz as an ingress does and retries a refused connection on another replica.
// Usage: node failover.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFileSync, spawn } from "node:child_process";
import { createServer } from "node:net";
import { createRequire } from "node:module";
import { schedule, runLater, shardLine } from "./shard.mjs";
import { recordResponses } from "./conform.mjs";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
const launch = { api: process.env.E2E_API_REPLICA, stream: process.env.E2E_STREAM_REPLICA };
if (!launch.api || !launch.stream) throw new Error("the failover gate needs E2E_API_REPLICA and E2E_STREAM_REPLICA (test/e2e/run.sh sets them)");
const scratch = process.env.E2E_SCRATCH;

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const failures = [];

function sql(text) {
  return execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();
}

// The worker's ffmpeg processes, from the process table.
function ffmpegRuns() {
  try {
    return execFileSync("pgrep", ["-af", `ffmpeg.*${scratch}`]).toString().trim().split("\n").filter(Boolean);
  } catch {
    return []; // pgrep exits 1 when nothing matches.
  }
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor(what, test, timeout = 30000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await sleep(200);
  }
  throw new Error(`timed out waiting for ${what}`);
}

function freePort() {
  return new Promise((resolve) => {
    const server = createServer();
    server.listen(0, "127.0.0.1", () => { const { port } = server.address(); server.close(() => resolve(port)); });
  });
}

// --- the router and the replicas ---------------------------------------------------

async function router(body) {
  const response = await fetch(`${base}/__router`, body ? { method: "PUT", body: JSON.stringify(body) } : {});
  return response.json();
}

const replicas = new Set();

// A replica of the role on a port of its own, up when its readiness answers, and not yet known to the router.
async function startReplica(role) {
  const port = await freePort();
  const child = spawn(launch[role], [String(port)], { stdio: "ignore" });
  const replica = { role, port, child, exited: new Promise((resolve) => child.once("exit", resolve)) };
  replicas.add(replica);
  await waitFor(`${role} replica ${port} to answer /healthz`, async () => {
    try { return (await fetch(`http://127.0.0.1:${port}/healthz`)).ok; } catch { return false; }
  }, 60000);
  return replica;
}

// Sends what the router serves for the role to these replicas, once it sees each ready.
async function route(role, list, throttle) {
  await router({ [role]: list.map((r) => r.port), ...(throttle === undefined ? {} : { throttle }) });
  await waitFor("the router to see every replica ready", async () => (await router())[role].filter((b) => list.some((r) => r.port === b.port)).every((b) => b.up));
}

async function stopAll() {
  for (const replica of replicas) replica.child.kill("SIGKILL");
  for (const replica of replicas) await replica.exited;
  replicas.clear();
  await router({ reset: true });
}

// --- the viewer --------------------------------------------------------------------

async function login(page) {
  await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", user);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
}

// A signed-in browser that records every request and answer with the replica that gave it, its
// WebSockets with what they received, and the player's progress reports.
async function viewer() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  recordResponses(page, base);
  const seen = { infos: [], answers: [], failed: [], reports: [], sockets: [], errors: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "POST" && /^\/Sessions\/Playing\/(Progress|Stopped)$/i.test(url.pathname)) {
      seen.reports.push({ at: Date.now(), kind: /Stopped/i.test(url.pathname) ? "Stopped" : "Progress", request, body: JSON.parse(request.postData() ?? "{}"), status: null });
    }
  });
  page.on("requestfailed", (request) => {
    if (!request.url().startsWith(base)) return;
    seen.failed.push({ at: Date.now(), method: request.method(), path: new URL(request.url()).pathname, error: request.failure()?.errorText });
    const report = seen.reports.find((r) => r.request === request);
    if (report) report.status = 0;
  });
  page.on("response", async (response) => {
    if (!response.url().startsWith(base)) return;
    const url = new URL(response.url());
    const answer = { at: Date.now(), status: response.status(), method: response.request().method(), path: url.pathname, url: response.url(), backend: Number(response.headers()["x-backend"]) || null, range: response.request().headers().range };
    seen.answers.push(answer);
    const report = seen.reports.find((r) => r.request === response.request());
    if (report) Object.assign(report, { status: answer.status, backend: answer.backend });
    if (response.request().method() === "POST" && /\/PlaybackInfo$/i.test(url.pathname)) {
      const reply = await response.json().catch(() => null);
      if (reply) seen.infos.push({ source: reply.MediaSources[0], session: reply.PlaySessionId });
    }
  });
  page.on("websocket", (ws) => {
    const socket = { url: ws.url(), openedAt: Date.now(), closedAt: null, frames: [], token: new URL(ws.url()).searchParams.get("ApiKey") ?? new URL(ws.url()).searchParams.get("api_key") };
    ws.on("framereceived", ({ payload }) => socket.frames.push(String(payload).slice(0, 200)));
    ws.on("close", () => { socket.closedAt = Date.now(); });
    seen.sockets.push(socket);
  });
  await login(page);
  return { context, page, seen };
}

const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);

// Where the player has data to, from the current time on.
const bufferedEnd = (page) => page.evaluate(() => {
  const v = document.querySelector("video");
  for (let i = 0; i < v.buffered.length; i++) if (v.buffered.start(i) <= v.currentTime + 0.5 && v.buffered.end(i) >= v.currentTime) return v.buffered.end(i);
  return v.currentTime;
});

async function play(page, library, title) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
}

async function advances(page, seconds) {
  const before = await time(page);
  await waitFor(`currentTime to pass ${before + seconds}`, async () => (await time(page)) > before + seconds, 60000);
}

// Plays on until the film is past `to`, failing at once on what a viewer would see go wrong: the
// error dialog, or the sign-in page.
// What the player and the router were doing, for a failure to say.
async function diagnosis(page, seen) {
  const video = await page.evaluate(() => {
    const v = document.querySelector("video");
    return v ? `video at ${v.currentTime.toFixed(1)} s, paused ${v.paused}, readyState ${v.readyState}, error ${v.error?.code ?? "none"}, buffered ${[...Array(v.buffered.length).keys()].map((i) => `${v.buffered.start(i).toFixed(0)}-${v.buffered.end(i).toFixed(0)}`)}, page ${location.hash}` : `no video, page ${location.hash}`;
  }).catch(() => "page gone");
  const last = seen.answers.filter((a) => /^\/videos\//i.test(a.path)).slice(-6).map((a) => `${a.status} ${a.path.split("/").slice(-2).join("/")} from ${a.backend}`);
  return `${video}; the last film requests: ${last.join("; ")}; failed: ${JSON.stringify(seen.failed.slice(-3))}`;
}

async function playsPast(page, to, timeout = 300000, seen) {
  const deadline = Date.now() + timeout;
  let now = -1;
  while (Date.now() < deadline) {
    if (await page.getByText("Playback Error").count()) throw new Error(`the viewer got a Playback Error dialog (the film was at ${now.toFixed(1)} s, wanted past ${to.toFixed(1)} s)`);
    if (await page.locator("#txtManualName").isVisible().catch(() => false)) throw new Error("the viewer is back at the sign-in page");
    now = await time(page);
    if (now > to) return now;
    await sleep(250);
  }
  throw new Error(`the film reached ${now.toFixed(1)} s, not past ${to.toFixed(1)} s: ${seen ? await diagnosis(page, seen) : ""}`);
}

const leave = async (page) => {
  await page.waitForFunction(() => location.hash.startsWith("#/video") && document.querySelector(".osdPositionText")?.textContent, null, { timeout: 15000 });
  const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 });
  await page.evaluate(() => window.history.back());
  const response = await stopped;
  if (response.status() !== 204) throw new Error(`Stopped answered ${response.status()}`);
  await response.finished();
};

// The scenarios of the gate. A failure names the scenario; every replica a scenario started is
// killed afterwards and the router goes back to the stack's own.
function scenario(name, run, options = {}) {
  return schedule(name, options, async () => {
    const began = Date.now();
    const v = await viewer();
    let passed = false;
    try {
      await run(v);
      passed = true;
    } catch (error) {
      failures.push(`${name}: ${error.message.split("\n")[0]}`);
      console.log(`FAIL  ${name}: ${error.message.split("\n")[0]} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-failover.png` }).catch(() => {});
    } finally {
      await v.context.close().catch(() => {});
      await stopAll();
    }
    await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 60000).catch(() => {
      failures.push(`${name}: ffmpeg still running afterwards`);
      console.log(`FAIL  ${name}: ffmpeg still running afterwards`);
      passed = false;
    });
    if (passed) console.log(`ok    ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  });
}

// --- api replica killed ------------------------------------------------------------

// What each api replica calls itself in session_sockets: a probe device signs in on it directly,
// opens a socket and reads the row it made.
async function replicaIds(list) {
  const ids = new Map();
  for (const replica of list) {
    const login = await fetch(`http://127.0.0.1:${replica.port}/Users/AuthenticateByName`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `MediaBrowser Client="probe", Device="probe", DeviceId="probe-${replica.port}", Version="1"` },
      body: JSON.stringify({ Username: user, Pw: password }),
    });
    const token = (await login.json()).AccessToken;
    const ws = new WebSocket(`ws://127.0.0.1:${replica.port}/socket?ApiKey=${token}`);
    await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = () => reject(new Error(`no socket on replica ${replica.port}`)); });
    const id = await waitFor("the probe socket's row", async () => sql(`SELECT replica FROM session_sockets WHERE token = '${token}'`));
    ids.set(replica.port, id);
    ws.close();
  }
  return ids;
}

const owner = (token) => sql(`SELECT COALESCE(ws_replica, '') FROM sessions WHERE token = '${token}'`);

await scenario("an api replica killed mid-film: the same player plays on, progress persists, the socket reconnects and ownership moves", async ({ page, seen }) => {
  const pair = [await startReplica("api"), await startReplica("api")];
  await route("api", pair);
  const ids = await replicaIds(pair);
  await play(page, "Transcode", "Api Kill Film");
  if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("the film should direct play");
  await advances(page, 2);
  await page.evaluate(() => { window.__video = document.querySelector("video"); });
  // The socket the page holds: whose it is is what sessions.ws_replica says.
  const first = seen.sockets.filter((s) => !s.closedAt && s.token).at(-1);
  if (!first) throw new Error("the page has no open /socket");
  const holding = await waitFor("ws_replica to name the replica that holds the viewer's socket", async () => owner(first.token));
  const victim = pair.find((r) => ids.get(r.port) === holding);
  if (!victim) throw new Error(`ws_replica ${holding} is none of the replicas of this scenario (${[...ids.values()]})`);
  const survivor = pair.find((r) => r !== victim);
  const asked = seen.infos.length;
  const reached = await time(page);
  const killedAt = Date.now();
  victim.child.kill("SIGKILL");
  await victim.exited;

  // The film plays at least 30 s past the kill, with no dialog and no sign-in page.
  await playsPast(page, reached + 30, 300000, seen);
  if (!(await page.evaluate(() => document.querySelector("video") === window.__video))) throw new Error("the player was replaced");
  if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked})`);

  // The socket reconnects, on a live replica, which opens with ForceKeepAlive.
  const again = await waitFor("an open /socket after the kill", async () => seen.sockets.find((s) => s.openedAt > killedAt && !s.closedAt), 60000);
  await waitFor("ForceKeepAlive on the new socket", async () => again.frames.some((f) => /ForceKeepAlive/.test(f)), 15000);
  // Ownership moved, and never names the killed replica again, also after the new socket closes.
  await waitFor("ws_replica to name the survivor", async () => owner(first.token) === ids.get(survivor.port), 30000);
  for (let i = 0; i < 20; i++) {
    if (owner(first.token) === holding) throw new Error("ws_replica named the killed replica again");
    await sleep(250);
  }

  // Progress persisted: every report after the kill that did not start on the dying replica was answered 2xx by a live one.
  const after = seen.reports.filter((r) => r.at > killedAt);
  if (after.filter((r) => r.kind === "Progress").length < 2) throw new Error(`the browser sent ${after.length} reports in the 30 s after the kill, expected Progress reports to go on`);
  const bad = after.filter((r) => !(r.status >= 200 && r.status < 300));
  if (bad.length) throw new Error(`reports after the kill were not answered 2xx: ${JSON.stringify(bad.map((r) => [r.kind, r.status, r.backend]))}`);
  if (after.some((r) => r.backend === victim.port)) throw new Error("a report after the kill was answered by the killed replica");
  const stoppedAt = await time(page);
  await leave(page);
  const stopped = seen.reports.filter((r) => r.kind === "Stopped").at(-1);
  if (!(stopped.status >= 200 && stopped.status < 300)) throw new Error(`Stopped answered ${stopped.status}`);
  const ticks = Number(sql("SELECT d.position_ticks FROM user_item_data d JOIN items i ON i.id = d.item_id WHERE i.name = 'Api Kill Film' ORDER BY d.last_played_at DESC NULLS LAST LIMIT 1"));
  const position = ticks / 1e7;
  if (Math.abs(position - stoppedAt) > 3) throw new Error(`the resume position is ${position.toFixed(1)} s, the viewer stopped at about ${stoppedAt.toFixed(1)} s`);
  if (position <= reached + 25) throw new Error(`the resume position ${position.toFixed(1)} s is not past where the film was at the kill (${reached.toFixed(1)} s)`);

  // Playing it again resumes there.
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Api Kill Film" }).first().click();
  const button = page.locator(".btnPlay[data-action=resume]:visible").first();
  await button.waitFor({ timeout: 30000 });
  await button.click();
  await waitFor("the film to play from the resume position", async () => (await time(page)) > position - 1, 60000);
  if ((await time(page)) > position + 15) throw new Error(`the film resumed at ${(await time(page)).toFixed(1)} s, not at ${position.toFixed(1)} s`);
  // And the device is not owned by the killed replica once its socket closes.
  await page.context().close();
  await waitFor("ws_replica to be clear or on a live replica", async () => owner(first.token) !== holding, 10000);
});

// --- stream replica killed or rolled -----------------------------------------------

const hlsPath = (path) => /^\/videos\/.*\/hls1\//i.test(path);

// Plays a film from two stream replicas, then `act` (kill or roll one), and checks the film goes on past what was
// buffered at that moment, on the same player, answered by replicas that are alive.
async function streamFailover({ page, seen }, { title, hls, act }) {
  const pair = [await startReplica("stream"), await startReplica("stream")];
  // A direct play is slowed to a download of tens of seconds, so the film is still being sent at the kill.
  await route("stream", pair, hls ? 0 : 1_500_000);
  await play(page, "Transcode", title);
  const source = seen.infos.at(-1).source;
  if (hls !== (source.TranscodingSubProtocol === "hls")) throw new Error(`the film should ${hls ? "transcode to HLS" : "direct play"}`);
  await advances(page, 2);
  await page.evaluate(() => { window.__video = document.querySelector("video"); });
  const asked = seen.infos.length;
  // The replica that is serving: the one that answered the latest request for the film.
  const serving = seen.answers.filter((a) => (hls ? hlsPath(a.path) : /^\/videos\/[^/]+\/stream/i.test(a.path)) && a.backend).at(-1)?.backend;
  const victim = pair.find((r) => r.port === serving) ?? pair[0];
  const survivor = pair.find((r) => r !== victim);
  const buffered = await bufferedEnd(page);
  // Direct play: the film is still being downloaded, not already in the player's buffer.
  if (!hls && buffered > 0.5 * (await page.evaluate(() => document.querySelector("video").duration))) throw new Error(`the player had ${buffered.toFixed(0)} s of the film at the kill: it was no longer being sent`);
  const at = Date.now();
  const outcome = await act({ victim, survivor, pair });
  // Past everything buffered at that moment: what plays next came from a replica that is alive.
  await playsPast(page, buffered + 3, 120000, seen);
  if (!(await page.evaluate(() => document.querySelector("video") === window.__video))) throw new Error("the player was replaced");
  if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked})`);
  if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
  const later = seen.answers.filter((a) => a.at > at && (hls ? hlsPath(a.path) : /^\/videos\/[^/]+\/stream/i.test(a.path)));
  if (later.some((a) => a.status < 400 && a.backend === victim.port && a.at > at + 1000 && outcome.kill)) throw new Error("the killed replica answered after it was killed");
  // A killed replica forces new requests, and HLS asks for a segment every few
  // seconds, so another replica answers. A rolled replica in direct play may
  // finish the range it is sending within the grace and carry the rest itself
  // (the criterion's "sent whole if they end within the grace"); the roll
  // scenario then checks that the replacement serves the film.
  const moved = outcome.kill || hls;
  if (moved && !later.some((a) => a.status < 400 && a.backend && a.backend !== victim.port)) throw new Error("no request for the film after that was answered by another replica");
  if (!hls) {
    if (outcome.kill && !later.some((a) => a.status === 206 && a.backend && a.backend !== victim.port)) throw new Error("no range was answered by another replica");
    if (ffmpegRuns().length) throw new Error("a direct play started a transcode");
  }
  return { victim, survivor, at, later, outcome };
}

for (const flavour of [{ name: "HLS", film: "Stream Kill Encode Film", hls: true }, { name: "direct play", film: "Stream Kill Direct Film", hls: false }]) {
  await scenario(`a stream replica killed while it serves the film: the film plays past what was buffered on the other replica: ${flavour.name}`, async (v) => {
    const { later, victim } = await streamFailover(v, {
      title: flavour.film, hls: flavour.hls,
      act: async ({ victim }) => { victim.child.kill("SIGKILL"); await victim.exited; return { kill: true }; },
    });
    // Whatever failed was on the dying replica: no other replica answered an error, and no request is left hanging.
    const bad = later.filter((a) => a.status >= 400 && a.backend !== victim.port);
    if (bad.length) throw new Error(`a live replica answered an error: ${JSON.stringify(bad.slice(0, 3).map((a) => [a.status, a.path, a.backend]))}`);
    await leave(v.page);
  });
}

for (const flavour of [{ name: "HLS", film: "Stream Roll Encode Film", hls: true }, { name: "direct play", film: "Stream Roll Direct Film", hls: false }]) {
  await scenario(`a stream replica rolled while it serves the film (SIGTERM, then a replacement): the film plays past what was buffered: ${flavour.name}`, async (v) => {
    let replacement;
    const { later, victim } = await streamFailover(v, {
      title: flavour.film, hls: flavour.hls,
      act: async ({ victim, pair }) => {
        victim.child.kill("SIGTERM");
        // It fails readiness first, and is still there to answer that.
        await waitFor("readiness of the replica that got SIGTERM to answer 503", async () => (await fetch(`http://127.0.0.1:${victim.port}/readyz`).then((r) => r.status, () => 0)) === 503, 5000);
        // As an orchestrator does: a replacement is started, and the router sees it once it is ready.
        replacement = await startReplica("stream");
        await router({ stream: [...pair.map((r) => r.port), replacement.port] });
        return { kill: false };
      },
    });
    // No playlist or segment request failed or was answered 400 or more, from any replica.
    if (flavour.hls) {
      const failed = v.seen.failed.filter((f) => /\/hls1\//i.test(f.path) || /m3u8/i.test(f.path));
      if (failed.length) throw new Error(`requests ended without an answer: ${JSON.stringify(failed.slice(0, 3))}`);
      const bad = v.seen.answers.filter((a) => a.at > v.seen.answers[0].at && (hlsPath(a.path) || /m3u8/i.test(a.path)) && a.status >= 400);
      if (bad.length) throw new Error(`playlist or segment requests answered 400 or more: ${JSON.stringify(bad.slice(0, 3).map((a) => [a.status, a.path, a.backend]))}`);
    }
    // The replacement serves the film once it is ready. The player may never ask
    // it: in direct play the draining replica can send the rest of the film within
    // its grace (as on CI's runners), so the scenario asks for a range of the film
    // itself, with the player's own URL and token, through the router, until the
    // replacement answers one.
    const filmUrl = v.seen.answers.filter((a) => /^\/videos\//i.test(a.path) && a.status < 400).at(-1)?.url;
    if (!filmUrl) throw new Error("the player asked for no film request to repeat");
    await waitFor("the replacement to serve the film", async () => {
      const answer = await fetch(filmUrl, { headers: { range: "bytes=0-1023" } }).catch(() => null);
      if (!answer) return false;
      await answer.arrayBuffer();
      return Number(answer.headers.get("x-backend")) === replacement.port && answer.status < 400;
    }, 120000);
    // And the one that got SIGTERM leaves by itself.
    await Promise.race([victim.exited, sleep(180000).then(() => { throw new Error("the replica that got SIGTERM was still running 180 s later"); })]);
    void later;
    await leave(v.page);
  });
}

await runLater();
await browser.close();
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  shardLine();
  process.exit(1);
}
console.log("\nPASS");
shardLine();
