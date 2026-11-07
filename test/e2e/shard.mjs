// Splits a gate's scenarios between runs, so CI can run a gate on several
// runners at once. E2E_SHARD=i/n (default 0/1) runs every n-th scenario() by
// call order, starting at the i-th. A scenario marked { stopsWorker: true }
// stops or kills the worker, or needs it gone, so it runs only on the last
// shard, after that shard's own share, in call order.
// Every gate ends with shardLine(): "shard i/n: ran k of T", where T counts every
// scenario() call before shard and E2E_ONLY filtering, so all shards print the
// same T and test/e2e/run.sh and test/check-shards.sh can check that between
// them the shards ran each scenario once.
const spec = process.env.E2E_SHARD ?? "0/1";
const [shard, of] = spec.split("/").map(Number);
if (!/^\d+\/\d+$/.test(spec) || of < 1 || shard >= of) throw new Error(`E2E_SHARD=${spec} is not i/n with 0 <= i < n`);
const only = process.env.E2E_ONLY ? new RegExp(process.env.E2E_ONLY, "i") : null;

let total = 0;
let ran = 0;
let dealt = 0;
const later = [];

// Counts a scenario and, if this shard owns it and E2E_ONLY allows it, runs
// execute (a stopsWorker scenario waits for runLater).
export async function schedule(name, options, execute) {
  total++;
  const mine = options.stopsWorker ? shard === of - 1 : dealt++ % of === shard;
  if (!mine || (only && !only.test(name))) return;
  ran++;
  // The name, for E2E_REPEAT's summary, which can't tell it from "name: why" otherwise.
  const start = () => (console.log(`run   ${name}`), execute());
  if (options.stopsWorker) later.push(start);
  else await start();
}

export async function runLater() {
  for (const execute of later) await execute();
}

// For a gate that is one sequence of steps and cannot be split (browse, play):
// it always runs whole.
export function wholeLine(count) {
  console.log(`shard 0/1: ran ${count} of ${count}`);
}

export function shardLine() {
  console.log(`shard ${shard}/${of}: ran ${ran} of ${total}`);
}
