// Plays out shard.mjs with scenarios that only print their names, for
// test/scripts_test.sh: E2E_SHARD=i/n node shard_test.mjs
import { runLater, schedule, shardLine } from "./shard.mjs";

const deal = (name, options = {}) => schedule(name, options, async () => console.log(`ran ${name}`));
for (let n = 1; n <= 7; n++) await deal(`s${n}`);
await deal("kill A", { stopsWorker: true });
await deal("s8");
await deal("kill B", { stopsWorker: true });
await deal("s9");
await runLater();
shardLine();
