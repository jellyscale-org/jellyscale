// node discover.mjs <server url>: asks @jellyfin/sdk's server discovery, as a client's
// "add server" screen does, which server it recommends at the address (JS-070). Prints
// the issues it found and fails unless the address is the best server and has none about
// its product name or version.
import { Jellyfin } from '@jellyfin/sdk'

const address = process.argv[2]
if (!address) {
  console.error('usage: node discover.mjs <server url>')
  process.exit(2)
}
const jellyfin = new Jellyfin({
  clientInfo: { name: 'jellyscale-sdknode', version: '1.0.0' },
  deviceInfo: { name: 'jellyscale-sdknode', id: 'jellyscale-sdknode' },
})
const candidates = await jellyfin.discovery.getRecommendedServerCandidates(address)
const best = jellyfin.discovery.findBestServer(candidates)
for (const c of candidates) {
  console.log(`candidate ${c.address}: score ${c.score}, issues [${c.issues.map((i) => i.constructor.name).join(', ')}]`)
}
if (!best) {
  console.error(`FAIL  @jellyfin/sdk's server discovery returns no server at ${address}: ${candidates.flatMap((c) => c.issues.map((i) => `${i.constructor.name} (ProductName ${c.systemInfo?.ProductName})`)).join(', ') || 'no candidate answered'}`)
  process.exit(1)
}
const issues = best.issues.filter((i) => i.constructor.name !== 'SlowResponseIssue')
if (issues.length > 0) {
  console.error(`FAIL  @jellyfin/sdk's server discovery finds issues with ${best.address}: ${issues.map((i) => i.constructor.name).join(', ')} (ProductName ${best.systemInfo?.ProductName}, Version ${best.systemInfo?.Version})`)
  process.exit(1)
}
console.log(`best server ${best.address}: ${best.systemInfo.ProductName} ${best.systemInfo.Version}`)
