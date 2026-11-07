export const meta = {
  name: 'build',
  description: 'Write the acceptance checks first, build until they pass, then verify by running it and one blind adversarial review',
  whenToUse: 'Every task from tasks/prd.json. Args: {task, acceptance: string[], tests: string[], id?: string, ux?: boolean, core?: boolean}',
  phases: [
    { title: 'Spec', detail: 'acceptance criteria become executable checks that fail today' },
    { title: 'Check', detail: 'review the design and whether the checks can catch a wrong build' },
    { title: 'Build', detail: 'one implementer, until the checks and tiers pass; checks are locked' },
    { title: 'Verify', detail: 'QA runs it; one blind adversarial review; architecture when core' },
    { title: 'Fix', detail: 'one round, evidenced findings only' },
  ],
}

// Why this shape (tasks/prd.json, PR "Replace deliver with build"): agents
// converge when they loop against checks that can fail, and reviewer comments
// are noisy. So the checks come first and are locked, a finding blocks only
// with executable evidence, minors never start a fix round, and a build that
// still fails after one fix round goes back to Spec: the checks missed something.

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          kind: { type: 'string', enum: ['defect', 'environment'], description: 'environment: the machine or tooling failed, not the change' },
          title: { type: 'string' },
          file: { type: 'string' },
          scenario: { type: 'string', description: 'Concrete input or state, and the wrong result' },
          evidenceKind: { type: 'string', enum: ['failing-test', 'command-output', 'capture-diff', 'reading'], description: 'reading means no executable evidence' },
          evidence: { type: 'string', description: 'The test name, or the command and its output, or the diff' },
        },
        required: ['severity', 'kind', 'title', 'scenario', 'evidenceKind', 'evidence'],
      },
    },
    summary: { type: 'string' },
  },
  required: ['findings', 'summary'],
}

const SPEC = {
  type: 'object',
  properties: {
    checks: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          criterion: { type: 'string', description: 'The acceptance criterion it checks' },
          file: { type: 'string', description: 'Path of the test or capture' },
          name: { type: 'string', description: 'Test or scenario name' },
          command: { type: 'string', description: 'How to run just this check' },
          failsToday: { type: 'string', description: 'The output showing it fails before the change, or why it cannot run yet' },
        },
        required: ['criterion', 'file', 'name', 'command', 'failsToday'],
      },
    },
    observed: { type: 'string', description: 'What was observed on a real Jellyfin 12.4 or client, and where it is recorded; or why nothing needed observing' },
    unchecked: { type: 'array', items: { type: 'string' }, description: 'Criteria with no executable check, and why' },
  },
  required: ['checks', 'observed', 'unchecked'],
}

const IMPLEMENTED = {
  type: 'object',
  properties: {
    done: { type: 'boolean' },
    report: { type: 'string', description: 'What changed, what ran and the real results' },
    disputes: { type: 'array', items: { type: 'string' }, description: 'Locked checks you believe are wrong, with evidence; you did not change them' },
    unfinished: { type: 'array', items: { type: 'string' } },
  },
  required: ['done', 'report', 'disputes', 'unfinished'],
}

const SNAPSHOT = {
  type: 'object',
  properties: { tree: { type: 'string' }, root: { type: 'string' } },
  required: ['tree', 'root'],
}

const LOCKED = {
  type: 'object',
  properties: { changed: { type: 'array', items: { type: 'string' }, description: 'Locked files that differ between the two trees' } },
  required: ['changed'],
}

const { task, acceptance = [], tests = [], id = '', ux = false, core = false } = args
if (!task) throw new Error('build needs args.task')

const criteria = acceptance.map((line) => `- ${line}`).join('\n')
const commands = tests.map((line) => `- ${line}`).join('\n')
const brief = `## Task${id ? ` ${id}` : ''}\n${task}\n\n## Acceptance criteria\n${criteria}\n\n## Test commands\n${commands}`

const MODELS = {
  'spec-writer': { model: 'sonnet' },
  implementer: { model: 'sonnet' },
  'architecture-reviewer': { model: 'sonnet' },
  'adversarial-reviewer': { model: 'sonnet' },
  qa: { model: 'haiku', effort: 'high' },
  'ux-reviewer': { model: 'haiku', effort: 'high' },
}
// Roles are read from their files, so a role added in this session works too.
const role = (name) => ({ agentType: 'general-purpose', ...MODELS[name] })
const read = (name) => `First read .claude/agents/${name}.md. It defines your role and its limits, and you must follow it exactly.\n\n`

let root = null
const snapshot = async (label, phase) => {
  const result = await agent('Run `bash tools/snapshot.sh` from the repository root and return the printed tree hash, and the absolute path of the repository root (`pwd`). Do nothing else.',
    { label, phase, schema: SNAPSHOT, model: 'haiku', effort: 'low' })
  if (!result) throw new Error(`snapshot failed: ${label}`)
  root = result.root
  return result.tree
}

const economy = 'Keep your context small: send command output to a file and read it with grep or tail, never whole. Report every blocker and major, and at most three minors. No style.'
const evidenceRule = 'A blocker or major needs executable evidence: a failing test, a command with its output, or a capture diff. A finding with only reading as evidence counts as minor.'

// ---- Spec: the criteria become checks before anything is built.
phase('Spec')
const base = await snapshot('snapshot base', 'Spec')
const specPrompt = (notes) => `${read('spec-writer')}${brief}${notes}\n\nWrite the acceptance checks for this task. Don't implement it and don't commit.`
let spec = await agent(specPrompt(''), { label: 'spec', phase: 'Spec', ...role('spec-writer'), schema: SPEC })
if (!spec) throw new Error('spec writer returned nothing')
let specTree = await snapshot('snapshot spec', 'Spec')

// ---- Check: design and checks reviewed together, once revised at most.
const checkList = (s) => s.checks.map((c, i) => `${i + 1}. ${c.criterion}\n   ${c.file}: ${c.name} (\`${c.command}\`)\n   Fails today: ${c.failsToday}`).join('\n')
const checkPrompt = (s, tree) => `${read('architecture-reviewer')}${brief}\n\n## Checks written for it\n${checkList(s)}\nObserved: ${s.observed}\nUnchecked: ${s.unchecked.join('; ') || 'none'}\n\nNothing is built yet; the checks are in \`git diff ${base} ${tree}\`. Review two things: the design (where this work should live and in what shape, against ARCHITECTURE.md and PRODUCT.md), and the checks: would each fail on a plausible wrong build, do they cover the criteria, was real behaviour observed where a protocol is involved? Never edit the working tree. ${evidenceRule}`
let design = null
for (let attempt = 1; attempt <= 2; attempt++) {
  phase('Check')
  design = await agent(checkPrompt(spec, specTree), { label: `check ${attempt}`, phase: 'Check', ...role('architecture-reviewer'), schema: FINDINGS })
  if (!design) throw new Error('check review returned nothing')
  const stops = design.findings.filter((f) => f.severity === 'blocker')
  if (!stops.length) break
  if (attempt === 2) return { clean: false, stoppedAt: 'Check', open: stops, spec, design, base, specTree }
  phase('Spec')
  const notes = `\n\n## Review of your first checks\n${stops.map((f) => `- ${f.title}: ${f.scenario}`).join('\n')}`
  spec = await agent(specPrompt(notes), { label: 'spec revised', phase: 'Spec', ...role('spec-writer'), schema: SPEC })
  if (!spec) throw new Error('spec writer returned nothing')
  specTree = await snapshot('snapshot spec revised', 'Spec')
}
const designNotes = design.findings.length
  ? `\n\n## Design review\n${design.findings.map((f) => `- [${f.severity}] ${f.title}: ${f.scenario}`).join('\n')}`
  : ''
const lockedFiles = [...new Set(spec.checks.map((c) => c.file))]
const locked = `\n\n## Locked checks\n${checkList(spec)}\n\nThese files are locked: ${lockedFiles.join(', ')}. Make the checks pass by building the task. Never edit, weaken, skip or delete a locked check. If you believe one is wrong, stop work on it and put it in disputes with your evidence.`

const lockedChanged = async (head, label) => {
  const result = await agent(`Run \`git diff --name-only ${specTree} ${head} -- ${lockedFiles.map((f) => `'${f}'`).join(' ')}\` in the repository root and return the files it lists. Do nothing else.`,
    { label, phase: 'Build', schema: LOCKED, model: 'haiku', effort: 'low' })
  return result ? result.changed : ['(could not check)']
}

// ---- Build.
phase('Build')
let built = await agent(`${read('implementer')}${brief}${designNotes}${locked}\n\nImplement this. Don't commit.`, { label: 'build', phase: 'Build', ...role('implementer'), schema: IMPLEMENTED })
if (!built) throw new Error('implementer returned nothing')
let head = await snapshot('snapshot build', 'Build')
let touched = await lockedChanged(head, 'locked checks')
if (touched.length || built.disputes.length) {
  return { clean: false, stoppedAt: 'Build', reason: touched.length ? 'locked checks changed' : 'checks disputed', touched, disputes: built.disputes, spec, base, specTree, head, build: built }
}

// What every reviewer is told: the change to read and how to stay out of its tree.
let reviewersNote = ''
const setNote = () => { reviewersNote = `Someone else has built the task above. You are reviewing it, not doing it: never edit, stash or mutate the working tree. Inspect the change with \`git diff ${base} ${head}\` (the checks were written first, in \`git diff ${base} ${specTree}\`). To build or test, use your own export: \`git archive ${head} | tar -x -C <dir in your scratchpad>\`, plus a symlink to web-dist for e2e, and run every command from inside it. Remove your export and anything you started before you finish; remove containers with \`docker rm -fv\`.` }

// ---- Verify: QA runs it, one adversarial reads to break it, architecture only for core changes.
const reviewers = (round) => {
  const list = [
    { key: 'qa', file: 'qa', focus: `${round === 1 ? 'Run the test commands, minus what a record covers, and check each acceptance criterion, including paths the checks skip.' : 'This round reviews fixes: rerun what the fixes touch and recheck the criteria they affect.'}\nHead tree: ${head}\nRecorded results: ${root}/.results/${head} (read-only; your role file says how to use them)` },
    { key: 'adversarial', file: 'adversarial-reviewer', focus: 'Break it where the locked checks do not look: concurrency across replicas and workers, partial failure, disconnects, leaks, client input, security, and whether the checks can really fail.' },
  ]
  if (core) list.push({ key: 'architecture', file: 'architecture-reviewer', focus: 'This change touches the scan, a store or job semantics: decide whether it is the right design, now that it is built.' })
  if (ux) list.push({ key: 'ux', file: 'ux-reviewer', focus: 'Review what viewers and operators experience. Check the affected flow in jellyfin-web once if you need to see it.' })
  return list
}
const verify = async (round) => {
  phase('Verify')
  const roles = reviewers(round)
  const results = await parallel(roles.map((r) => () =>
    agent(`${read(r.file)}${brief}\n\n${reviewersNote}\n\n${r.focus}\n\n${evidenceRule} ${economy}`, { label: `${r.key} (round ${round})`, phase: 'Verify', ...role(r.file), schema: FINDINGS })
      .then((result) => ({ key: r.key, result }))))
  const answered = results.filter((x) => x && x.result)
  const missing = roles.map((r) => r.key).filter((k) => !answered.some((x) => x.key === k))
  const findings = answered.flatMap((x) => x.result.findings.map((f) => ({ ...f, reviewer: x.key })))
  // The evidence gate: reading alone never blocks, and the environment is not the change.
  for (const f of findings) if (f.severity !== 'minor' && f.evidenceKind === 'reading') f.severity = 'minor'
  return { findings, missing }
}

setNote()
const history = []
let result = await verify(1)
history.push({ round: 1, ...result })
const blocking = (r) => r.findings.filter((f) => f.severity !== 'minor' && f.kind === 'defect')
let open = blocking(result)
log(`Verify 1: ${result.findings.length} finding(s), ${open.length} evidenced and blocking`)

// ---- Fix: one round, then back to Spec if anything evidenced remains.
if (open.length || result.missing.length) {
  phase('Fix')
  const list = open.map((f, i) => `${i + 1}. [${f.severity}] ${f.title} (${f.file ?? 'no file'})\nScenario: ${f.scenario}\nEvidence (${f.evidenceKind}): ${f.evidence}`).join('\n\n')
  const fixed = await agent(`${read('implementer')}${brief}${locked}\n\n## Evidenced findings\n${list}\n\nFix each real defect and add a test that catches it. If a finding is wrong, say why with evidence rather than changing code. Don't commit.`, { label: 'fix', phase: 'Fix', ...role('implementer'), schema: IMPLEMENTED })
  if (!fixed) throw new Error('fixer returned nothing')
  built = fixed
  head = await snapshot('snapshot fix', 'Fix')
  touched = await lockedChanged(head, 'locked checks after fix')
  if (touched.length || fixed.disputes.length) {
    return { clean: false, stoppedAt: 'Fix', reason: touched.length ? 'locked checks changed' : 'checks disputed', touched, disputes: fixed.disputes, spec, base, specTree, head, history }
  }
  setNote()
  result = await verify(2)
  history.push({ round: 2, ...result })
  open = blocking(result)
  log(`Verify 2: ${result.findings.length} finding(s), ${open.length} evidenced and blocking`)
}

const all = history.flatMap((h) => h.findings)
return {
  clean: open.length === 0 && result.missing.length === 0,
  // Evidenced blockers after the one fix round mean the checks missed something.
  backToSpec: open.length > 0,
  id, base, specTree, head,
  open,
  unreviewed: result.missing,
  environment: all.filter((f) => f.kind === 'environment'),
  backlog: all.filter((f) => f.severity === 'minor' && f.kind === 'defect'),
  spec,
  design: design.findings,
  build: built,
  rounds: history.map((h) => ({ round: h.round, findings: h.findings.map((f) => ({ reviewer: f.reviewer, severity: f.severity, kind: f.kind, evidenceKind: f.evidenceKind, title: f.title })) })),
}
