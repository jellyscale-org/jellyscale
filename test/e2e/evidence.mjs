// What a failed scenario or step leaves behind, so the failure can be read without running it again (JS-160): the
// browser's whole console at every level and its page errors, a screenshot taken at the failure, and the viewer's
// record (every request, answer and socket with its frames, and one timeline of all of them, each timed). They go to
// $E2E_FAILURES (test/e2e/run.sh sets it to <logs>/failures, which CI uploads on failure), in files named by gate
// and scenario, and by iteration under E2E_REPEAT, so no failure overwrites another's. For every request to the
// server that the browser reports failed, the output also says what the test router answered for it (router.mjs keeps
// the log), or that it never received it. A scenario that passes keeps nothing and prints nothing.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";

const recorders = new Set();
// A token wherever it is written: a query value (ApiKey=, api_key=, token=), a header or a JSON key (X-Emby-Token: ...,
// "AccessToken": "..."), the Token="..." of an Authorization value.
// The separator and quotes are written plainly, JSON-escaped (\" ) or percent-encoded (%3D, %3A, %22, %5C%22); the value
// ends at a terminator in any of those forms.
const quote = String.raw`(?:\\*["']|%5[Cc]%22|%22|%27)?`;
const separator = String.raw`\s*(?:[=:]|%3[Dd]|%3[Aa])\s*`;
const secret = new RegExp(String.raw`(api_?key|access_?token|x-emby-token|x-mediabrowser-token|token)(${quote}${separator}${quote})(?:(?!%22|%27|%2[Cc]|%26|%7[Dd])[^&\s"',;}\\])+`, "gi");
export const redact = (text) => String(text).replace(secret, "$1$2…");

// Starts recording what the context's pages do, for the requests to base: call it once for each context a gate opens.
// A page the scenario opens later is recorded too.
export function watch(context, base) {
  const recorder = { base, began: Date.now(), events: [], requests: [], answers: [], failed: [], sockets: [], console: [], errors: [], context, late: [], closed: false, open: new Set() };
  recorders.add(recorder);
  const since = () => Date.now() - recorder.began;
  const note = (what) => recorder.events.push({ at: since(), what: redact(what) });
  // The same origin as base, not the same beginning: 127.0.0.1:4104 is not 127.0.0.1:41041.
  const origin = new URL(base).origin;
  const ours = (url) => { try { return new URL(url).origin === origin; } catch { return false; } };
  const attach = (page) => {
    page.on("console", (message) => {
      const where = message.location().url ? ` (${redact(message.location().url)}:${message.location().lineNumber})` : "";
      const line = `${message.type()}: ${redact(message.text())}${where}`;
      recorder.console.push({ at: since(), line });
      note(`console ${line.slice(0, 300)}`);
    });
    page.on("pageerror", (error) => {
      recorder.errors.push({ at: since(), line: redact(error.stack ?? error.message) });
      note(`page error ${error.message}`);
    });
    page.on("crash", () => note("the page crashed"));
    page.on("request", (request) => {
      if (!ours(request.url())) return;
      const entry = { at: since(), started: Date.now(), method: request.method(), url: redact(request.url()), range: request.headers().range };
      recorder.requests.push(entry);
      request.evidence = entry;
      recorder.open.add(request);
      note(`request ${entry.method} ${entry.url}${entry.range ? ` ${entry.range}` : ""}`);
    });
    page.on("response", (response) => {
      if (!ours(response.url())) return;
      const headers = response.headers();
      const entry = { at: since(), status: response.status(), method: response.request().method(), url: redact(response.url()), routed: headers["x-routed-to"], backend: headers["x-backend"] };
      recorder.answers.push(entry);
      note(`answer ${entry.status} ${entry.method} ${entry.url} from ${entry.routed ?? "?"}`);
    });
    page.on("requestfinished", (request) => recorder.open.delete(request));
    page.on("requestfailed", (request) => {
      recorder.open.delete(request);
      if (!ours(request.url())) return;
      const url = new URL(request.url());
      // The earlier of when this side saw the request and when the browser says it began it (an event can be late).
      const began = request.timing().startTime;
      const started = Math.min(request.evidence?.started ?? Date.now(), began > 0 ? began : Infinity);
      const entry = { at: since(), started, failedAt: Date.now(), method: request.method(), path: url.pathname, target: url.pathname + url.search, origin: url.origin, error: request.failure()?.errorText };
      recorder.failed.push(entry);
      note(`request failed ${entry.method} ${redact(entry.target)} ${entry.error}`);
    });
    page.on("websocket", (ws) => {
      const socket = { url: redact(ws.url()), openedAt: since(), closedAt: null, received: [], sent: [] };
      recorder.sockets.push(socket);
      note(`socket opened ${socket.url}`);
      ws.on("framereceived", ({ payload }) => { socket.received.push({ at: since(), payload: redact(String(payload).slice(0, 300)) }); note(`socket ${socket.url.split("?")[0]} received ${String(payload).slice(0, 120)}`); });
      ws.on("framesent", ({ payload }) => { socket.sent.push({ at: since(), payload: redact(String(payload).slice(0, 300)) }); note(`socket ${socket.url.split("?")[0]} sent ${String(payload).slice(0, 120)}`); });
      ws.on("close", () => { socket.closedAt = since(); note(`socket closed ${socket.url.split("?")[0]}`); });
    });
  };
  for (const page of context.pages()) attach(page);
  context.on("page", attach);
  context.on("close", () => { recorder.closed = true; });
  return recorder;
}

// Forgets what the contexts closed before startedAt recorded: a scenario that has ended has no failure to keep.
export function forgetBefore(startedAt) {
  for (const recorder of recorders) if (recorder.closed && recorder.began < startedAt) recorders.delete(recorder);
}

// A screenshot of a page that is about to close, for a failure that is found after it has: the failure keeps it.
export async function rescue(page) {
  const recorder = [...recorders].find((candidate) => candidate.context === page.context());
  const picture = await page.screenshot().catch(() => null);
  if (recorder && picture) recorder.late.push(picture);
}

const slug = (name) => name.toLowerCase().replace(/[^a-z0-9]+/g, "-").slice(0, 70).replace(/^-|-$/g, "");

// The files' common name: the gate, the iteration under E2E_REPEAT, the scenario, and a hash of its whole name so that
// two names alike in their first words keep their own files. A second failure with the same name in one run gets
// ".again<n>" after the stem, and the pictures of a failure are "<stem>.png", "<stem>-2.png" and so on, so no name
// of one failure is another's.
function stemFor(name) {
  const gate = process.env.E2E_GATE ?? "gate";
  const repeat = Number(process.env.E2E_REPEAT ?? 1) > 1 ? `-iteration${process.env.E2E_ITERATION ?? 1}` : "";
  const dir = process.env.E2E_FAILURES ?? `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-failures`;
  mkdirSync(dir, { recursive: true });
  const stem = `${dir}/${gate}${repeat}-${slug(name)}-${createHash("sha1").update(name).digest("hex").slice(0, 8)}`;
  for (let n = 1; ; n++) {
    const candidate = n === 1 ? stem : `${stem}.again${n}`;
    if (!existsSync(`${candidate}.console.txt`)) return candidate;
  }
}

// What the router said of each request, from its log. A log that cannot be read says why.
async function routerAnswers(origins) {
  const logs = new Map();
  for (const origin of origins) {
    try {
      const response = await fetch(`${origin}/__router/requests`, { signal: AbortSignal.timeout(3000) });
      logs.set(origin, { log: (await response.json()).requests });
    } catch (error) {
      logs.set(origin, { error: error.name === "TimeoutError" ? "no answer in 3 s" : error.message });
    }
  }
  return logs;
}

// A router entry can be the failed request's only if the router received it when or after the browser began it (a few
// ms of clock and event-order slack) and it had not ended before then: an earlier identical request is another one.
const slack = 25;
const answers = (failed, entry) => entry.method === failed.method && entry.url === failed.target
  && entry.at >= failed.started - slack && entry.at <= failed.failedAt + slack && !(entry.endedAt && entry.endedAt < failed.started - slack);

function describeAnswer(failed, entry) {
  if (!entry) return "the router received no such request";
  if (!entry.status) return `the router received it ${Math.max(0, entry.at - failed.started)} ms after the browser sent it and sent no answer`;
  const endings = { "client closed": ", and the client closed it before the end", "upstream failed": ", and the replica's response broke", "fault cut": ", and the router cut it on purpose (a fault)" };
  return `the router answered ${entry.status} from ${entry.routed} (backend port ${entry.backend ?? "none"})${endings[entry.ended] ?? ""}`;
}

// Prints the line of each request the browser reported failed in the failing step or scenario, with the router's answer.
// A router entry answers one request only.
async function printFailedRequests(failures) {
  if (!failures.length) return;
  const logs = await routerAnswers(new Set(failures.map((failed) => failed.origin)));
  const used = new Set();
  for (const failed of failures) {
    const { log, error } = logs.get(failed.origin);
    const near = (log ?? []).filter((entry) => !used.has(entry) && answers(failed, entry));
    near.sort((a, b) => Math.abs(a.at - failed.started) - Math.abs(b.at - failed.started));
    if (near[0]) used.add(near[0]);
    const answer = log ? describeAnswer(failed, near[0]) : `the router's log could not be read (${error})`;
    console.log(`      requestfailed ${failed.method} ${redact(failed.target)}: ${failed.error}; ${answer}`);
  }
}

// Playwright delivers a request's failure after the page's own answer to the step may have: wait, on the failure path
// only and for at most 1.5 s, until every request the live pages sent has finished or failed.
async function settle(live) {
  await new Promise((resolve) => setTimeout(resolve, 50));
  const deadline = Date.now() + 1500;
  while (live.some((recorder) => !recorder.closed && recorder.open.size) && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

// Keeps the evidence of a failure of the named scenario or step, which began at startedAt, for every context that is
// open or was opened since (a scenario's second viewer, or a viewer the scenario closed), and prints where it is and
// the failed requests. The pictures come from the pages open now and from rescue.
// A gate whose steps share one page (browse, play, cors) has the earlier steps' events in the same recorder: the
// failed requests printed and kept as "failed" are those since startedAt, and the console and the timeline, kept
// whole, say where the step began; the failed requests before it are apart, as "failedEarlier".
export async function keepFailure(name, startedAt = 0) {
  try {
    await keep(name, startedAt);
  } catch (error) {
    // The failure being kept is the one that matters: a gate goes on past an evidence it could not write.
    console.log(`      evidence could not be kept: ${error.message}`);
  }
}

async function keep(name, startedAt) {
  const live = [...recorders].filter((recorder) => !recorder.closed || recorder.began >= startedAt);
  if (!live.length) return;
  await settle(live);
  const stem = stemFor(name);
  const files = [];
  const write = (suffix, content) => { writeFileSync(`${stem}${suffix}`, content); files.push(suffix); };
  const consoleLines = [];
  const record = [];
  const inStep = [];
  for (const [index, recorder] of live.entries()) {
    const label = live.length > 1 ? `viewer ${index + 1}: ` : "";
    const stepAt = Math.max(0, startedAt - recorder.began);
    const marker = (at) => `--- ${label}the failing step began here, ${at} ms after the page was first watched ---`;
    const lines = [...recorder.console.map(({ at, line }) => ({ at, text: `${label}+${at} ms ${line}` })), ...recorder.errors.map(({ at, line }) => ({ at, text: `${label}+${at} ms page error: ${line}` }))].sort((a, b) => a.at - b.at);
    if (stepAt > 0) lines.push({ at: stepAt, text: marker(stepAt) });
    consoleLines.push(...lines.sort((a, b) => a.at - b.at).map(({ text }) => text));
    const timeline = recorder.events.map(({ at, what }) => ({ at, text: `${at} ms ${what}` }));
    if (stepAt > 0) timeline.push({ at: stepAt, text: marker(stepAt) });
    timeline.sort((a, b) => a.at - b.at);
    inStep.push(...recorder.failed.filter((failed) => failed.failedAt >= startedAt));
    record.push({
      viewer: index + 1,
      began: new Date(recorder.began).toISOString(),
      stepBeganAt: stepAt,
      requests: recorder.requests.map(({ at, method, url, range }) => ({ at, method, url, range })),
      answers: recorder.answers,
      failed: recorder.failed.filter((failed) => failed.failedAt >= startedAt).map(({ at, method, target, error }) => ({ at, method, url: redact(target), error })),
      failedEarlier: recorder.failed.filter((failed) => failed.failedAt < startedAt).map(({ at, method, target, error }) => ({ at, method, url: redact(target), error })),
      sockets: recorder.sockets,
      timeline: timeline.map(({ text }) => text),
    });
    const pages = recorder.closed ? [] : recorder.context.pages();
    const shots = await Promise.all(pages.map((page) => page.screenshot().catch(() => null)));
    for (const [n, picture] of [...shots, ...recorder.late].filter(Boolean).entries()) {
      write(`${live.length > 1 ? `-viewer${index + 1}` : ""}${n > 0 ? `-${n + 1}` : ""}.png`, picture);
    }
    recorder.late = [];
  }
  write(".console.txt", consoleLines.length ? `${consoleLines.join("\n")}\n` : "the console and the page errors were empty\n");
  write(".record.json", JSON.stringify(record, null, 2));
  console.log(`      evidence: ${stem}{${files.join(",")}}`);
  await printFailedRequests(inStep);
}
