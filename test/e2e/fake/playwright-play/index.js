// A stand-in for playwright-core that plays a film (test/e2e/fake/run-gate.sh with FAKE_PLAYWRIGHT=playwright-play):
// enough of a page for the failover gate's two stream-roll scenarios to run from the sign-in to the end,
// so that what the gate does after its last scenario can be timed. Not a browser: the "video" counts
// seconds from the Play click, 3 s of it buffered ahead, and the page reports answers to its requests
// (a PlaybackInfo, then film requests) round-robin over the stream replicas the stand-in router holds.
const vm = require("node:vm");

function page(base) {
  const handlers = {};
  const on = (name, fn) => { (handlers[name] ||= []).push(fn); return target; };
  const emit = (name, ...args) => Promise.all((handlers[name] ?? []).map((fn) => fn(...args)));
  let hls = false;
  let startedAt = 0;
  let timer;
  let turn = 0;
  const now = () => (startedAt ? (Date.now() - startedAt) / 1000 : 0);
  const video = { get currentTime() { return now(); }, paused: false, readyState: 4, error: null, duration: 3600,
    buffered: { length: 1, start: () => 0, end: () => now() + 3 } };
  const context = vm.createContext({
    document: { querySelector: (sel) => (sel === "video" && startedAt ? video : null) },
    location: { hash: "#/video" }, history: { back() {} },
  });
  context.window = context;

  const request = (method, path, body) => ({ method: () => method, headers: () => ({}), postData: () => body, url: () => base + path });
  const answer = (method, path, status, backend, json, body) => {
    const req = request(method, path, body);
    return emit("response", {
      url: () => base + path, status: () => status, request: () => req,
      headers: () => ({ "x-backend": String(backend ?? ""), ...(json ? { "content-type": "application/json" } : {}) }),
      json: async () => json, text: async () => JSON.stringify(json ?? {}), body: async () => Buffer.alloc(0),
    });
  };
  const backends = async () => {
    const table = await fetch(`${base}/__router`).then((r) => r.json()).catch(() => ({}));
    return (table.stream ?? []).map((b) => b.port);
  };
  const play = async () => {
    startedAt = Date.now();
    await answer("POST", "/Items/1/PlaybackInfo", 200, 0, { PlaySessionId: "s", MediaSources: [hls ? { TranscodingSubProtocol: "hls" } : {}] }, "{}");
    timer = setInterval(async () => {
      const ports = await backends();
      if (!ports.length) return;
      const port = ports[turn++ % ports.length];
      await (hls ? answer("GET", `/videos/1/hls1/main/${turn}.ts`, 200, port) : answer("GET", "/videos/1/stream?static=true", 206, port));
    }, 200);
  };

  const locator = (selector, options = {}) => {
    const self = {
      first: () => self,
      click: async () => {
        if (/cardText/.test(selector)) hls = /Encode/.test(options.hasText ?? "");
        if (/btnPlay/.test(selector)) await play();
      },
      waitFor: async () => {},
      isVisible: async () => false,
      count: async () => 0,
    };
    return self;
  };
  const target = {
    on, once: on, off: () => target, removeListener: () => target,
    url: () => "about:blank",
    goto: async () => {}, waitForSelector: async () => {}, fill: async () => {}, click: async () => {}, screenshot: async () => {},
    close: async () => { clearInterval(timer); }, setViewportSize: async () => {}, bringToFront: async () => {}, setDefaultTimeout: () => {},
    locator, getByText: () => ({ count: async () => 0 }),
    evaluate: async (fn, arg) => vm.runInContext(`(${fn})(${JSON.stringify(arg ?? null)})`, context),
    waitForFunction: async () => {},
    waitForResponse: async () => ({ status: () => 204, finished: async () => {} }),
    context: () => ({ close: async () => { clearInterval(timer); } }),
  };
  return target;
}

// A page's film requests stop with its context, as the browser's would; a page left running would keep the gate's process alive.
const base = () => process.argv[2];
const pages = [];
const newPage = async () => { const p = page(base()); pages.push(p); return p; };
const closeAll = async () => { for (const p of pages) await p.close(); };
const context = () => ({ newPage, close: closeAll, on() {} });
exports.chromium = {
  launch: async () => ({ newContext: async () => context(), newPage, close: closeAll, on() {} }),
};
