export const meta = {
  name: 'deliver',
  description: 'Implement a scoped change, then run blind adversarial, QA, Product and optional UX reviews, fixing until clean',
  whenToUse: 'Non-trivial changes. Args: {task, acceptance: string[], tests: string[], ux?: boolean, maxRounds?: number}',
  phases: [
    { title: 'Implement', detail: 'implementation subagent' },
    { title: 'Review', detail: 'two blind adversarial, QA, Product, optional UX' },
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

const { task, acceptance = [], tests = [], ux = false, maxRounds = 3, roleFiles = false } = args
if (!task) throw new Error('deliver needs args.task')

const criteria = acceptance.map((line) => `- ${line}`).join('\n')
const commands = tests.map((line) => `- ${line}`).join('\n')
const brief = `## Task\n${task}\n\n## Acceptance criteria\n${criteria}\n\n## Test commands\n${commands}`

// Agent definitions register at session start, so a session that created them
// sets roleFiles and loads each role by reading its file. That keeps the role
// text but loses its tool limits, which is why the review loop below checks the
// tree didn't change under reviewers.
const MODELS = {
  implementer: { model: 'sonnet' },
  'adversarial-reviewer': { model: 'sonnet' },
  qa: { model: 'haiku', effort: 'max' },
  'product-reviewer': { model: 'haiku', effort: 'max' },
  'ux-reviewer': { model: 'haiku', effort: 'max' },
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

// Reviewers get the task, the diff range and the docs. The implementer's report
// is deliberately absent, and each reviewer runs without seeing the others.
const reviewers = (base, head) => {
  const diff = `Inspect the change with \`git diff ${base} ${head}\`. It's between two tree snapshots, so nothing is committed; read current files from the working directory.`
  const roles = [
    { key: 'adversarial-1', agentType: 'adversarial-reviewer', focus: 'Concentrate on failure: concurrency across replicas, partial failure, disconnects and resource leaks.' },
    { key: 'adversarial-2', agentType: 'adversarial-reviewer', focus: 'Concentrate on correctness and trust: client input, protocol behavior against the evidence in the task, security, and whether the tests can really fail. Also check this is the simplest code that does the job.' },
    { key: 'qa', agentType: 'qa', focus: 'Run everything and check each acceptance criterion.' },
    { key: 'product', agentType: 'product-reviewer', focus: 'Check the change against PRODUCT.md and the task.' },
  ]
  if (ux) roles.push({ key: 'ux', agentType: 'ux-reviewer', focus: 'Review what viewers and operators experience.' })
  return roles.map((role) => ({
    ...role,
    prompt: `${brief}\n\n${diff}\n\n${role.focus}`,
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

phase('Implement')
const base = await snapshot('snapshot before')
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
  // Both adversarial reviewers always return, so fix code gets the same
  // scrutiny as the original.
  const again = new Set(['qa', 'adversarial-1', 'adversarial-2', ...open.map((finding) => finding.reviewer), ...unreviewed])
  roles = reviewers(base, head).filter((role) => again.has(role.key))
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
