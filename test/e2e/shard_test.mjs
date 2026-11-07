// Plays out shard.mjs with scenarios that only print their names, for
// test/scripts_test.sh: E2E_SHARD=i/n node shard_test.mjs
import { runLater, schedule, shardLine } from "./shard.mjs";

const scenario = (name, options = {}) => schedule(name, options, async () => console.log(`ran ${name}`));
for (let n = 1; n <= 7; n++) await scenario(`s${n}`);
await scenario("kill A", { stopsWorker: true });
await scenario("s8");
await scenario("kill B", { stopsWorker: true });
await scenario("s9");
await runLater();
shardLine();
