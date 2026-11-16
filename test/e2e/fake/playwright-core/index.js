// A stand-in for playwright-core, for test/e2e/fake/run-gate.sh: it plays out a gate's scenario list,
// its shard arithmetic and its failure handling without a browser. A page lets the sign-in form through
// (goto, waitForSelector, fill, click) and refuses everything else, so each scenario fails at its first
// real step, at once, and the gate has to report that as a failed scenario and go on.
const allowed = new Set(["goto", "waitForSelector", "fill", "click", "screenshot", "close", "setViewportSize", "bringToFront", "setDefaultTimeout"]);
const quiet = new Set(["on", "once", "off", "removeListener", "addListener", "removeAllListeners"]);

function page() {
  const target = {};
  return new Proxy(target, {
    get(_, name) {
      if (typeof name === "symbol" || name === "then") return undefined;
      if (quiet.has(name)) return () => target;
      if (name === "url") return () => "about:blank";
      if (allowed.has(name)) return async () => undefined;
      return () => { throw new Error(`fake browser: page.${name} is not available`); };
    },
  });
}

const context = () => ({ newPage: async () => page(), close: async () => {}, on() {} });
exports.chromium = {
  launch: async () => ({ newContext: async () => context(), newPage: async () => page(), close: async () => {}, on() {} }),
};
