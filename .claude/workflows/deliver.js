export const meta = {
  name: 'deliver',
  description: 'Implement a scoped change, then run blind adversarial, QA, Product and optional UX reviews, fixing until clean',
  whenToUse: 'Non-trivial changes. Args: {task, acceptance: string[], tests: string[], ux?: boolean, maxRounds?: number}',
  phases: [
    { title: 'Design', detail: 'architecture review before anything is built' },
    { title: 'Implement', detail: 'implementation subagent' },
    { title: 'Review', detail: 'architecture, two blind adversarial, QA, Product, optional UX' },
    { title: 'Fix', detail: 'fix blocking findings, then re-review' },
  ],
}

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          title: { type: 'string' },
          file: { type: 'string' },
          scenario: { type: 'string', description: 'Concrete input or state, and the wrong result' },
          evidence: { type: 'string', description: 'What you ran or read that shows it' },
        },
        required: ['severity', 'title', 'scenario', 'evidence'],
      },
    },
    summary: { type: 'string' },
  },
  required: ['findings', 'summary'],
}

const IMPLEMENTED = {
  type: 'object',
  properties: {
    done: { type: 'boolean' },
    report: { type: 'string', description: 'What changed, what ran and the real results' },
    unfinished: { type: 'array', items: { type: 'string' } },
  },
  required: ['done', 'report', 'unfinished'],
}

const SNAPSHOT = {
  type: 'object',
  properties: { tree: { type: 'string' } },
  required: ['tree'],
}

// base continues earlier work: reviewers then see the whole change since that
// snapshot, not just this run's fixes.
// designReview: false skips the review before building, for a fix pass on work
// whose design has been reviewed.
const { task, acceptance = [], tests = [], ux = false, maxRounds = 2, roleFiles = false, base: earlierBase, designReview = true } = args
if (!task) throw new Error('deliver needs args.task')

const criteria = acceptance.map((line) => `- ${line}`).join('\n')
const commands = tests.map((line) => `- ${line}`).join('\n')
let brief = `## Task\n${task}\n\n## Acceptance criteria\n${criteria}\n\n## Test commands\n${commands}`

// Agent definitions register at session start, so a session that created them
// sets roleFiles and loads each role by reading its file. That keeps the role
// text but loses its tool limits, which is why the review loop below checks the
// tree didn't change under reviewers.
const MODELS = {
  implementer: { model: 'sonnet' },
  'architecture-reviewer': { model: 'sonnet' },
  'adversarial-reviewer': { model: 'sonnet' },
  qa: { model: 'haiku', effort: 'high' },
  'product-reviewer': { model: 'haiku', effort: 'high' },
  'ux-reviewer': { model: 'haiku', effort: 'high' },
}
const roleOptions = (role) => ({ agentType: roleFiles ? 'general-purpose' : role, ...MODELS[role] })
const rolePrefix = (role) => (roleFiles
  ? `First read .claude/agents/${role}.md. It defines your role and its limits, and you must follow it exactly.\n\n`
  : '')

const snapshot = async (label) => {
  const result = await agent(
    'Run `bash tools/snapshot.sh` from the repository root and return the printed tree hash. Do nothing else.',
    { label, phase: 'Implement', schema: SNAPSHOT, model: 'haiku', effort: 'low' })
  if (!result) throw new Error(`snapshot failed: ${label}`)
  return result.tree
}

// What a review costs is mostly the context it rereads on every tool call, and
// Haiku bills a prompt over 100K tokens at five times the rate. Long logs read
// whole were most of the bill, so every reviewer keeps output out of its context.
const economy = 'Keep your context small. Send command output to a file under your export and read it with grep or tail, never whole. Stop when each finding has its evidence. Report every blocker and major, and at most three minors.'

// Reviewers get the task, the diff range and the docs. The implementer's report
// is deliberately absent, and each reviewer runs without seeing the others.
// A later round reviews fixes, so QA reruns what the fixes touch, not everything.
const reviewers = (base, head, round = 1) => {
  // The brief reads as instructions to implement, so say plainly that a reviewer
  // isn't to carry them out. Parallel reviewers share one working tree.
  const diff = `Someone else has already done the task above. You are reviewing it, not doing it: never edit, stash or mutate the working tree. Inspect the change with \`git diff ${base} ${head}\`, between two tree snapshots, so nothing is committed. To build, test or mutate, use your own export: \`git archive ${head} | tar -x -C <dir in your scratchpad>\`, plus a symlink to web-dist for the e2e test. /tmp has a small quota: use test media no larger than you need, and delete your export and media before you finish.`
  const roles = [
    { key: 'architecture', agentType: 'architecture-reviewer', focus: 'Decide whether this is the right design, now that it is built.' },
    { key: 'adversarial-1', agentType: 'adversarial-reviewer', focus: 'Concentrate on failure: concurrency across replicas, partial failure, disconnects and resource leaks.' },
    { key: 'adversarial-2', agentType: 'adversarial-reviewer', focus: 'Concentrate on correctness and trust: client input, protocol behavior against the evidence in the task, security, and whether the tests can really fail. Also check this is the simplest code that does the job.' },
    {
      key: 'qa',
      agentType: 'qa',
      focus: round === 1
        ? 'Run the test commands once and check each acceptance criterion.'
        : 'This round reviews fixes to work you saw before. Run the tests of the packages and gates the fixes touch, and recheck the criteria they affect; run a full suite only if the fixes reach across packages.',
    },
    { key: 'product', agentType: 'product-reviewer', focus: 'Check the change against PRODUCT.md and the task, by reading. Run no tests and no browser.' },
  ]
  if (ux) roles.push({ key: 'ux', agentType: 'ux-reviewer', focus: 'Review what viewers and operators experience. Check the affected flow in jellyfin-web once if you need to see it; run no test suite.' })
  return roles.map((role) => ({
    ...role,
    prompt: `${brief}\n\n${diff}\n\n${role.focus} ${economy}`,
  }))
}

const runReviews = async (roles, round) => {
  const results = await parallel(roles.map((role) => () =>
    agent(rolePrefix(role.agentType) + role.prompt, {
      label: `${role.key} (round ${round})`,
      phase: 'Review',
      ...roleOptions(role.agentType),
      schema: FINDINGS,
    }).then((result) => ({ role, result }))))
  const failed = results.filter((entry) => !entry || !entry.result)
  if (failed.length) log(`${failed.length} reviewer(s) returned nothing, which counts as not reviewed`)
  return results
}

// Cheapest place to catch a wrong design: nothing is built yet. A blocker stops
// the run, and anything else reaches the implementer and reviewers in the brief.
if (designReview) {
  phase('Design')
  const design = await agent(`${rolePrefix('architecture-reviewer')}${brief}\n\nNothing is built yet. You are reviewing the task's design, not doing the task. Read the code it would touch, and decide where this work should live and in what shape. Never edit the working tree.`, {
    label: 'design review',
    phase: 'Design',
    ...roleOptions('architecture-reviewer'),
    schema: FINDINGS,
  })
  if (!design) throw new Error('design review returned nothing')
  const stops = design.findings.filter((finding) => finding.severity === 'blocker')
  if (stops.length) return { clean: false, rounds: 0, stoppedAtDesign: true, open: stops, design }
  if (design.findings.length) {
    const notes = design.findings.map((finding) => `- ${finding.title}: ${finding.scenario}`).join('\n')
    brief += `\n\n## Design review\nRaised before this was built. Address these or say why not.\n${notes}`
  }
}

phase('Implement')
const base = earlierBase ?? await snapshot('snapshot before')
let implementation = await agent(`${rolePrefix('implementer')}${brief}\n\nImplement this. Don't commit.`, {
  label: 'implement',
  phase: 'Implement',
  ...roleOptions('implementer'),
  schema: IMPLEMENTED,
})
if (!implementation) throw new Error('implementer returned nothing')

let head = await snapshot('snapshot after')
let roles = reviewers(base, head)
const history = []
let open = []
let unreviewed = []

for (let round = 1; round <= maxRounds; round++) {
  phase('Review')
  const results = await runReviews(roles, round)
  if (roleFiles) {
    const after = await snapshot(`check round ${round}`)
    if (after !== head) throw new Error(`the tree changed during review round ${round}: ${head} to ${after}`)
  }
  const answered = results.filter((entry) => entry && entry.result)
  unreviewed = roles.map((role) => role.key).filter((key) => !answered.some((entry) => entry.role.key === key))
  const findings = answered.flatMap((entry) =>
    entry.result.findings.map((finding) => ({ ...finding, reviewer: entry.role.key })))
  history.push({ round, reviewed: roles.map((role) => role.key), findings })
  open = findings.filter((finding) => finding.severity !== 'minor')
  log(`Round ${round}: ${findings.length} finding(s), ${open.length} blocking`)
  if (!open.length && !unreviewed.length) break
  if (round === maxRounds) break

  phase('Fix')
  const list = open.map((finding, i) =>
    `${i + 1}. [${finding.severity}] ${finding.title} (${finding.file ?? 'no file'})\nScenario: ${finding.scenario}\nEvidence: ${finding.evidence}`).join('\n\n')
  const fixed = await agent(`${rolePrefix('implementer')}${brief}\n\n## Review findings\n${list}\n\nFix each real defect and add a test that catches it. If a finding is wrong, say why with evidence rather than changing code. Don't commit.`, {
    label: `fix round ${round}`,
    phase: 'Fix',
    ...roleOptions('implementer'),
    schema: IMPLEMENTED,
  })
  if (!fixed) throw new Error('fixer returned nothing')
  implementation = fixed
  head = await snapshot(`snapshot round ${round}`)
  // Reviewers who raised something, and QA always, look again. They stay blind:
  // the diff is the whole change, and they don't see the fixer's explanation.
  // Architecture and both adversarial reviewers always return, so fix code gets
  // the same scrutiny as the original.
  const again = new Set(['qa', 'architecture', 'adversarial-1', 'adversarial-2', ...open.map((finding) => finding.reviewer), ...unreviewed])
  roles = reviewers(base, head, round + 1).filter((role) => again.has(role.key))
}

const minor = history.flatMap((round) => round.findings).filter((finding) => finding.severity === 'minor')
return {
  clean: open.length === 0 && unreviewed.length === 0,
  rounds: history.length,
  base,
  head,
  open,
  unreviewed,
  minor,
  implementation,
  history: history.map(({ round, reviewed, findings }) => ({ round, reviewed, count: findings.length })),
}
