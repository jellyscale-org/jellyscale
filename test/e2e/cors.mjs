// JS-144 (D-016): a page served from an origin other than the server's, which is what Fluster (a Tauri WebView) and
// any web client hosted elsewhere are. The page here is a blank one on another port; everything it does it does with
// fetch, as such a client does, and the token goes only in the Authorization header. The stack is the usual one: two
// api replicas, a stream replica and a router, each a process of its own. Playwright cannot see a preflight, so the
// Chrome DevTools protocol records every answer (type "Preflight" for those) with the replica the router sent it to.
// One sequence of steps, so it always runs whole. Each step stands on its own: the steps after the sign-in use a
// token obtained outside the page (here, by node), so that one that fails does not hide the next.
// Usage: node cors.mjs <base_url> <user> <password>   (E2E_MOVIES: the movies directory the stack scans)
import { createRequire } from "node:module";
import http from "node:http";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { wholeLine } from "./shard.mjs";
import { recordResponses } from "./conform.mjs";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
const movies = process.env.E2E_MOVIES;

// The page's own origin: a server of one blank page on a port of its own.
const origin = await new Promise((resolve) => {
  const server = http.createServer((req, res) => res.end("<!doctype html><title>another origin</title>")).listen(0, "127.0.0.1", () => {
    resolve(`http://localhost:${server.address().port}`);
  });
  server.unref();
});

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const context = await browser.newContext({ viewport: { width: 800, height: 600 } });
const page = await context.newPage();
recordResponses(page, base);
page.on("console", (message) => { if (process.env.E2E_DEBUG) console.log(`page: ${message.text()}`); });
await page.goto(`${origin}/`);

const lower = (headers) => Object.fromEntries(Object.entries(headers ?? {}).map(([k, v]) => [k.toLowerCase(), v]));
const sent = new Map();
const answers = [];
const cdp = await context.newCDPSession(page);
await cdp.send("Network.enable");
cdp.on("Network.requestWillBeSent", (e) => sent.set(e.requestId, { url: e.request.url, method: e.request.method, headers: lower(e.request.headers) }));
cdp.on("Network.responseReceived", (e) => {
  const request = sent.get(e.requestId) ?? {};
  const headers = lower(e.response.headers);
  answers.push({ kind: e.type, method: request.method, url: e.response.url, status: e.response.status, request: request.headers ?? {}, headers, routed: headers["x-routed-to"] });
});
cdp.on("Network.loadingFailed", (e) => answers.push({ kind: e.type, method: sent.get(e.requestId)?.method, url: sent.get(e.requestId)?.url, status: 0, failed: e.errorText, request: {}, headers: {} }));
// The protocol's events reach node a moment after the page has its answer, so a step starts and ends a moment late.
const settle = () => new Promise((resolve) => setTimeout(resolve, 400));
const mark = async () => { await settle(); return answers.length; };
const ours = async (from) => { await settle(); return answers.slice(from).filter((a) => a.url?.startsWith(base)); };

const failures = [];
let steps = 0;
const only = process.env.E2E_ONLY ? new RegExp(process.env.E2E_ONLY, "i") : null;
async function step(name, action) {
  steps++;
  if (only && !only.test(name)) return;
  const began = Date.now();
  try {
    // A step that hangs (a page that waits for a media buffer that never fills) is a failure, not a stuck gate.
    let timer;
    await Promise.race([action(), new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("the step hung for 180 s")), 180000); })]).finally(() => clearTimeout(timer));
    console.log(`ok    ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name}: ${error.message.split("\n")[0]} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  }
}
const need = (condition, message) => { if (!condition) throw new Error(message); };
const describe = (a) => `${a.kind} ${a.method} ${new URL(a.url).pathname} -> ${a.status}${a.failed ? ` ${a.failed}` : ""}`;

// A device of its own for each sign-in: signing in again from a device ends the token it had.
const authorization = (token, device = "cors-gate") => `MediaBrowser Client="cors", Device="Chromium", DeviceId="${device}", Version="1"${token ? `, Token="${token}"` : ""}`;

// A token from outside the page, for the steps that are not about signing in.
async function signIn() {
  const response = await fetch(`${base}/Users/AuthenticateByName`, {
    method: "POST", headers: { Authorization: authorization(), "Content-Type": "application/json" },
    body: JSON.stringify({ Username: user, Pw: password }),
  });
  const body = await response.json();
  return { token: body.AccessToken, userId: body.User.Id };
}
const outside = await signIn();

// The ids of a film, asked by node with the outside token: the steps of the page that are not about browsing do not
// stand on the page's own browsing.
const film = async (name, library) => {
  const get = async (path) => (await fetch(`${base}${path}`, { headers: { Authorization: authorization(outside.token) } })).json();
  const lib = (await get("/UserViews")).Items.find((v) => v.Name === library);
  return (await get(`/Items?ParentId=${lib.Id}&Recursive=true&IncludeItemTypes=Movie`)).Items.find((i) => i.Name.startsWith(name));
};

// What every request of a step that carries a token must look like: the token in the header and in no URL, and every
// preflight answered 2xx by the replica the router chose for the route.
async function checkRequests(from, replicas) {
  const mine = await ours(from);
  need(mine.length > 0, "the step made no request to the server");
  for (const a of mine) {
    need(a.status > 0, `a request failed (${describe(a)})`);
    if (a.kind === "Preflight") {
      need(a.status >= 200 && a.status < 300, `a preflight was refused: ${describe(a)}`);
    } else {
      need(/^MediaBrowser /.test(a.request.authorization ?? ""), `no Authorization: MediaBrowser on ${describe(a)}`);
      need(!/[?&](apikey|api_key)=/i.test(a.url) && !a.url.includes(outside.token), `the token is in the URL of ${describe(a)}`);
    }
    need(replicas.includes(a.routed), `${describe(a)} was answered by ${a.routed}, want ${replicas.join(" or ")}`);
  }
  return mine;
}

// Everything the page does goes through this, which returns what the page could read.
const inPage = (fn, arg) => page.evaluate(fn, arg);

await step("the page reads GET /System/Info/Public from another origin", async () => {
  const from = await mark();
  const read = await inPage(async (base) => {
    const response = await fetch(`${base}/System/Info/Public`);
    return { origin: location.origin, status: response.status, body: await response.json() };
  }, base);
  need(read.origin !== new URL(base).origin, `the page is on ${read.origin}, the server's own origin`);
  need(read.status === 200 && typeof read.body.Version === "string", `read ${read.status} ${JSON.stringify(read.body).slice(0, 80)}`);
  const mine = await ours(from);
  need(mine.every((a) => a.kind !== "Preflight"), "a simple GET was preflighted");
  need(mine.length === 1 && ["api1", "api2"].includes(mine[0].routed), `answered by ${mine.map((a) => a.routed)}`);
});

await step("a wrong password is read as a 401, not as a failed connection", async () => {
  const from = await mark();
  const read = await inPage(async ({ base, auth }) => {
    try {
      const response = await fetch(`${base}/Users/AuthenticateByName`, {
        method: "POST", headers: { Authorization: auth, "Content-Type": "application/json" },
        body: JSON.stringify({ Username: "admin", Pw: "not-the-password" }),
      });
      return { status: response.status };
    } catch (error) {
      return { error: String(error) };
    }
  }, { base, auth: authorization() });
  need(read.status === 401, `the page read ${JSON.stringify(read)}, want a 401 it can see`);
  const mine = await ours(from);
  need(mine.some((a) => a.kind === "Preflight" && a.status >= 200 && a.status < 300), `no preflight answered 2xx (${mine.map(describe)})`);
  need(mine.every((a) => ["api1", "api2"].includes(a.routed)), "an api request went elsewhere");
});

await step("the page signs in with POST /Users/AuthenticateByName and its token works", async () => {
  const from = await mark();
  const read = await inPage(async ({ base, auth, user, password }) => {
    const response = await fetch(`${base}/Users/AuthenticateByName`, {
      method: "POST", headers: { Authorization: auth, "Content-Type": "application/json" }, body: JSON.stringify({ Username: user, Pw: password }),
    });
    const body = await response.json();
    const me = await fetch(`${base}/Users/Me`, { headers: { Authorization: auth.replace(/$/, `, Token="${body.AccessToken}"`) } });
    return { status: response.status, token: !!body.AccessToken, me: me.status };
  }, { base, auth: authorization(null, "cors-signin"), user, password });
  need(read.status === 200 && read.token && read.me === 200, `sign-in ${read.status}, token ${read.token}, GET /Users/Me ${read.me}`);
  const mine = await ours(from);
  need(mine.filter((a) => a.kind === "Preflight").length >= 1 && mine.every((a) => a.status < 300 && ["api1", "api2"].includes(a.routed)), mine.map(describe).join("; "));
});

// The film and library ids, asked by the page with the outside token.
const api = (path, token = outside.token) => inPage(async ({ base, path, auth }) => {
  const response = await fetch(`${base}${path}`, { headers: { Authorization: auth } });
  return { status: response.status, body: await response.json() };
}, { base, path, auth: authorization(token) });

await step("the page lists the libraries and one library's films with the token in the header only", async () => {
  const from = await mark();
  const views = await api("/UserViews");
  need(views.status === 200 && views.body.Items.length >= 2, `UserViews ${views.status}`);
  const library = views.body.Items.find((v) => v.Name === "Movies");
  need(library, `no Movies library in ${views.body.Items.map((v) => v.Name)}`);
  const films = await api(`/Items?ParentId=${library.Id}&Recursive=true&IncludeItemTypes=Movie`);
  need(films.status === 200 && films.body.Items.some((i) => i.Name === "Alpha Film"), `Items ${films.status}`);
  const mine = await checkRequests(from, ["api1", "api2"]);
  need(mine.filter((a) => a.kind === "Preflight").length >= 2, "each request should have been preflighted (an Authorization header)");
});

await step("the page direct plays a film from bytes it fetched by script, through a seek", async () => {
  const alpha = await film("Alpha Film", "Movies");
  need(alpha, "no Alpha Film");
  const id = alpha.Id.replace(/-/g, "");
  const path = `/Videos/${id}/stream?static=true&mediaSourceId=${id}`;
  const file = readFileSync(`${movies}/Alpha Film (2021)/Alpha Film (2021).mkv`);
  const from = await mark();
  const read = await inPage(async ({ base, path, auth, start, end }) => {
    const get = (range) => fetch(`${base}${path}`, { headers: { Authorization: auth, Range: `bytes=${range}` } });
    const middle = await get(`${start}-${end}`);
    const bytes = new Uint8Array(await middle.arrayBuffer());
    const digest = [...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes))].map((b) => b.toString(16).padStart(2, "0")).join("");
    return { status: middle.status, digest, range: middle.headers.get("Content-Range"), accept: middle.headers.get("Accept-Ranges"), length: middle.headers.get("Content-Length") };
  }, { base, path, auth: authorization(outside.token), start: Math.floor(file.length / 2), end: Math.floor(file.length / 2) + 99999 });
  const start = Math.floor(file.length / 2), end = start + 99999;
  need(read.status === 206, `the middle range answered ${read.status}`);
  need(read.digest === createHash("sha256").update(file.subarray(start, end + 1)).digest("hex"), "the 206 holds other bytes than the file's");
  need(read.range === `bytes ${start}-${end}/${file.length}` && read.accept === "bytes" && read.length === "100000",
    `the page reads Content-Range ${read.range}, Accept-Ranges ${read.accept}, Content-Length ${read.length}`);
  // The film, in pieces the page fetched, played from a blob.
  const played = await inPage(async ({ base, path, auth, size }) => {
    const parts = [];
    for (let at = 0; at < size; at += 262144) {
      const piece = await fetch(`${base}${path}`, { headers: { Authorization: auth, Range: `bytes=${at}-${Math.min(size, at + 262144) - 1}` } });
      if (piece.status !== 206) return { error: `piece at ${at}: ${piece.status}` };
      parts.push(await piece.arrayBuffer());
    }
    const video = document.createElement("video");
    video.muted = true;
    video.src = URL.createObjectURL(new Blob(parts, { type: "video/x-matroska" }));
    document.body.append(video);
    window.picture = () => ({ t: video.currentTime, frames: video.getVideoPlaybackQuality().totalVideoFrames, seeking: video.seeking, ready: video.readyState, duration: video.duration, error: video.error?.message });
    window.seekTo = (t) => { video.currentTime = t; };
    video.play().catch((e) => { window.playError = String(e); });
    return {};
  }, { base, path, auth: authorization(outside.token), size: file.length });
  need(!played.error, played.error);
  const wait = async (what, test) => {
    const deadline = Date.now() + 30000;
    for (;;) {
      const state = await inPage(() => window.picture());
      if (test(state)) return state;
      need(Date.now() < deadline, `timed out waiting for ${what}: ${JSON.stringify(state)}`);
      await new Promise((r) => setTimeout(r, 200));
    }
  };
  const first = await wait("the picture to advance", (s) => s.t > 1.5 && s.frames > 10);
  await inPage(() => window.seekTo(20));
  const after = await wait("the picture to advance after a seek", (s) => s.t > 21.5 && !s.seeking && s.frames > first.frames);
  need(after.frames > first.frames, "no new frames after the seek");
  const mine = await ours(from);
  need(mine.filter((a) => a.kind !== "Preflight").every((a) => a.routed === "stream" && /Videos/i.test(a.url) && (a.status === 206)), `a media request went to ${mine.map((a) => `${a.routed}:${a.status}`)}`);
  need(mine.some((a) => a.kind === "Preflight" && a.status < 300 && a.routed === "stream"), "no preflight of the stream was answered 2xx by the stream replica");
  need(mine.every((a) => !/[?&](apikey|api_key)=/i.test(a.url) && /^MediaBrowser /.test(a.request.authorization ?? "") || a.kind === "Preflight"), "a request had no Authorization header, or the token was in a URL");
});

await step("the page plays a film that transcodes, with the Authorization header on every playlist and segment, through a far seek", async () => {
  const zeta = await film("Cors Film", "Transcode");
  need(zeta, "no Cors Film in the Transcode library");
  const from = await mark();
  const read = await inPage(async ({ base, id, userId, auth }) => {
    const text = async (url) => {
      const response = await fetch(url, { headers: { Authorization: auth } });
      if (!response.ok) throw new Error(`${response.status} ${url.replace(/\?.*/, "")}`);
      return response.text();
    };
    const profile = {
      Name: "cors", MaxStreamingBitrate: 120000000,
      DirectPlayProfiles: [{ Container: "mp4", Type: "Video", VideoCodec: "h264", AudioCodec: "aac" }],
      TranscodingProfiles: [{ Container: "mp4", Type: "Video", AudioCodec: "aac", VideoCodec: "h264", Context: "Streaming", Protocol: "hls", MaxAudioChannels: "2", MinSegments: "1", BreakOnNonKeyFrames: true }],
    };
    const info = await fetch(`${base}/Items/${id}/PlaybackInfo?userId=${userId}`, {
      method: "POST", headers: { Authorization: auth, "Content-Type": "application/json" },
      body: JSON.stringify({ DeviceProfile: profile, MaxStreamingBitrate: 120000000, EnableDirectPlay: true, EnableDirectStream: false, EnableTranscoding: true, AutoOpenLiveStream: false }),
    }).then((r) => r.json());
    const source = info.MediaSources[0];
    if (!source.TranscodingUrl) return { error: `no TranscodingUrl: direct play ${source.SupportsDirectPlay}` };
    // ApiKey comes off the TranscodingUrl: the token stays in the header.
    const masterUrl = new URL(`${base}${source.TranscodingUrl}`);
    masterUrl.searchParams.delete("ApiKey");
    masterUrl.searchParams.delete("api_key");
    console.log("transcoding url", source.TranscodingUrl.replace(/\?.*/, ""));
    const master = await text(masterUrl);
    console.log("master", master.length);
    const mainLine = master.split("\n").find((l) => /^main\.m3u8/.test(l));
    const codecs = /CODECS="([^"]+)"/.exec(master)?.[1];
    const mainUrl = new URL(mainLine, masterUrl);
    const main = await text(mainUrl);
    console.log("main", main.length);
    const lines = main.split("\n");
    const init = new URL(/#EXT-X-MAP:URI="([^"]+)"/.exec(main)[1], mainUrl);
    const segments = [];
    lines.forEach((line, i) => {
      const length = /^#EXTINF:([\d.]+)/.exec(line);
      if (length) segments.push({ length: Number(length[1]), url: new URL(lines[i + 1], mainUrl) });
    });
    let at = 0;
    for (const s of segments) { s.start = at; at += s.length; }
    const bytes = async (url) => (await fetch(url, { headers: { Authorization: auth } }).then(async (r) => { if (!r.ok) throw new Error(`${r.status} ${url.pathname}`); return r.arrayBuffer(); }));
    const media = new MediaSource();
    const video = document.createElement("video");
    video.muted = true;
    video.src = URL.createObjectURL(media);
    document.body.append(video);
    await new Promise((r) => media.addEventListener("sourceopen", r, { once: true }));
    const buffer = media.addSourceBuffer(`video/mp4; codecs="${codecs}"`);
    media.duration = at; // So that a seek past what is buffered is not clamped to its end.
    const append = (data) => new Promise((resolve, reject) => { buffer.addEventListener("updateend", resolve, { once: true }); buffer.addEventListener("error", () => reject(new Error("append failed")), { once: true }); buffer.appendBuffer(data); });
    console.log("codecs", codecs);
    await append(await bytes(init));
    console.log("init appended");
    window.hls = { video, segments, buffer, media, append, bytes };
    // The segments carry the film's own timestamps, which need not start at 0 (hls.js reads them and moves the timeline):
    // the first one says where, and the rest of the film is shifted by that.
    await append(await bytes(segments[0].url));
    const first = buffer.buffered.start(0);
    await new Promise((resolve) => { buffer.addEventListener("updateend", resolve, { once: true }); buffer.remove(0, first + 1000); });
    buffer.timestampOffset = -first;
    for (const s of segments.slice(0, 3)) await append(await bytes(s.url));
    window.hlsPicture = () => ({ t: video.currentTime, seeking: video.seeking, ready: video.readyState, error: video.error?.message, buffered: [...Array(video.buffered.length).keys()].map((i) => [video.buffered.start(i), video.buffered.end(i)]) });
    video.play().catch((e) => { window.playError = String(e); });
    return { segments: segments.length, masterHasToken: /apikey|api_key/i.test(master), mainHasToken: /apikey|api_key/i.test(main), total: at };
  }, { base, id: zeta.Id, userId: outside.userId, auth: authorization(outside.token) });
  need(!read.error, read.error);
  need(read.segments >= 6, `the playlist lists ${read.segments} segments`);
  need(!read.masterHasToken && !read.mainHasToken, "a playlist line carries a token though the request carried none (invariant 17)");
  const wait = async (what, test) => {
    const deadline = Date.now() + 40000;
    for (;;) {
      const state = await inPage(() => window.hlsPicture());
      if (test(state)) return state;
      need(Date.now() < deadline, `timed out waiting for ${what}: ${JSON.stringify(state)}`);
      await new Promise((r) => setTimeout(r, 200));
    }
  };
  await wait("time to advance", (s) => s.t > 1.5 && !s.seeking);
  // Beyond what is buffered (three segments): the player fetches the segment of the new place, as a player does.
  const far = await inPage(async () => {
    const { video, segments, append, bytes } = window.hls;
    const k = Math.floor(segments.length * 0.7);
    video.currentTime = segments[k].start + 0.2;
    for (const s of segments.slice(k, k + 3)) await append(await bytes(s.url));
    return { k, start: segments[k].start };
  });
  await wait("time to advance after the seek", (s) => s.t > far.start + 1.5 && !s.seeking);
  const mine = await checkRequests(from, ["api1", "api2", "stream"]);
  const media = mine.filter((a) => /\/Videos\//i.test(a.url));
  need(media.length >= 8 && media.every((a) => a.routed === "stream"), `media requests: ${media.map((a) => a.routed)}`);
  need(media.some((a) => a.kind === "Preflight") && media.some((a) => /master\.m3u8/i.test(a.url)) && media.some((a) => /main\.m3u8/i.test(a.url)), "master, main and a preflight were not all seen");
  need(media.some((a) => new RegExp(`/hls1/main/${far.k}\\.`).test(a.url)), `the segment at the new place (${far.k}) was not fetched`);
});

// JS-145 (D-020): what jellyfin-web shows a viewer whose sign-in is refused. The name is invented, so it needs no account (an
// unknown name is refused like a real one) and no other scenario signs in with it; it is new on each run, and the bounds
// are counted in Postgres, so the 20 wrong passwords it takes are its own. The stack leaves `trustedProxies` out, so the
// per-address bound is off and the per-name bound is what refuses.
await step("a refused sign-in shows jellyfin-web's 403 text and stays on the sign-in form", async () => {
  const name = `refused-${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`;
  const attempt = async (device) => {
    const response = await fetch(`${base}/Users/AuthenticateByName`, {
      method: "POST", headers: { Authorization: authorization(null, device), "Content-Type": "application/json" },
      body: JSON.stringify({ Username: name, Pw: "not-the-password" }),
    });
    await response.text();
    return response;
  };
  for (let i = 1; i <= 20; i++) {
    const response = await attempt(`refused-${name}-${i}`);
    need(response.status === 401, `wrong password ${i}: ${response.status}, want 401`);
  }
  const refused = await attempt(`refused-${name}-21`);
  const retryAfter = refused.headers.get("retry-after");
  need(refused.status === 403, `the 21st attempt answered ${refused.status}, want 403`);
  need(/^\d+$/.test(retryAfter ?? "") && +retryAfter >= 1 && +retryAfter <= 900, `Retry-After ${retryAfter}, want whole seconds from 1 to 900`);
  const web = await context.newPage();
  try {
    await web.goto(`${base}/web/index.html#/login.html`);
    await web.waitForSelector("#txtManualName", { timeout: 60000 });
    await web.fill("#txtManualName", name);
    await web.fill("#txtManualPassword", "not-the-password");
    await web.click("button[type=submit]");
    const text = "You are not authorized to access the server at this time. Please contact your server administrator for more information.";
    await web.locator(".toast", { hasText: text }).first().waitFor({ timeout: 15000 }).catch(() => {});
    const shown = await web.evaluate(() => [...document.querySelectorAll(".toast, .dialog, .dialogContent")].map((e) => e.innerText.trim()).filter(Boolean));
    console.log(`recorded: jellyfin-web showed ${JSON.stringify(shown)} for a 403 with Retry-After ${retryAfter}`);
    need(shown.some((t) => t.includes(text)), `jellyfin-web showed ${JSON.stringify(shown)}, want the 403 text`);
    need(await web.locator("#txtManualName").isVisible(), "the page left the sign-in form");
  } finally {
    await web.close();
  }
});

await browser.close();
console.log(failures.length ? `\n${failures.length} of ${steps} steps failed:\n${failures.join("\n")}` : `\nall ${steps} steps passed`);
wholeLine(steps);
process.exit(failures.length ? 1 : 0);
