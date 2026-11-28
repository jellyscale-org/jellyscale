export const meta = {
  name: 'build',
  description: 'Write the checks first, review them, build until they pass, then verify by running it and one blind review',
  whenToUse: 'Every task that is not small (docs/delivery.md). Args: {task, acceptance: string[], tests: string[], id?: string, root?: string, domains?: string[], ux?: boolean, core?: boolean, strong?: boolean, resolved?: {files: string[], note: string, settled?: string[], unlock?: string[]}}',
  phases: [
    { title: 'Spec', detail: 'acceptance criteria become executable checks that fail today' },
    { title: 'Check', detail: 'one blind review of the design and the checks, before anything is built' },
    { title: 'Build', detail: 'one implementer, until the checks pass; the checks are locked' },
    { title: 'Verify', detail: 'QA runs it; one blind adversarial review; UX when the task has ux' },
    { title: 'Fix', detail: 'one round, evidenced findings only' },
  ],
}

// Why this shape. Agents converge when they loop against checks that can fail,
// and reviewer comments are noisy. So the checks come first and are locked, a
// finding blocks only with evidence that runs, minors never start a fix round,
// and a build that still fails after one fix round goes back to Spec: the
// checks missed something. docs/delivery.md describes each step.

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
          evidenceKind: { type: 'string', enum: ['failing-test', 'command-output', 'measurement', 'reading'], description: 'reading means no evidence that runs' },
          evidence: { type: 'string', description: 'The test name, or the command and its output' },
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
          file: { type: 'string', description: 'Path of the test' },
          name: { type: 'string', description: 'Test or scenario name' },
          command: { type: 'string', description: 'How to run just this check' },
          failsToday: { type: 'string', description: 'The output showing it fails before the change, or why it cannot run yet' },
          wrongBuild: { type: 'string', description: 'The one-line wrong build this check would fail on' },
        },
        required: ['criterion', 'file', 'name', 'command', 'failsToday', 'wrongBuild'],
      },
    },
    criteriaChanges: { type: 'array', items: { type: 'string' }, description: 'Each criterion you sharpened, before and after, and why. Never weaker.' },
    observed: { type: 'string', description: 'What was observed on a player or an app and where it is recorded, or why nothing needed observing' },
    unchecked: { type: 'array', items: { type: 'string' }, description: 'Criteria with no executable check, and why' },
    answers: { type: 'array', items: { type: 'string' }, description: 'For a revision after review: each finding, and the check that now covers it, or why you disagree, with evidence' },
  },
  required: ['checks', 'criteriaChanges', 'observed', 'unchecked'],
}

const IMPLEMENTED = {
  type: 'object',
  properties: {
    done: { type: 'boolean' },
    report: { type: 'string', description: 'What changed, what ran and the real results' },
    disputes: { type: 'array', items: { type: 'string' }, description: 'Locked checks you believe are wrong, with evidence; you did not change them' },
    unfinished: { type: 'array', items: { type: 'string' } },
    reviewAnswers: { type: 'array', items: { type: 'string' }, description: 'Each blocker or major of the review before the build: what you built for it, or why not, with evidence' },
  },
  required: ['done', 'report', 'disputes', 'unfinished', 'reviewAnswers'],
}

const SNAPSHOT = {
  type: 'object',
  properties: { tree: { type: 'string' }, root: { type: 'string' } },
  required: ['tree', 'root'],
}

const FILES = {
  type: 'object',
  properties: { changed: { type: 'array', items: { type: 'string' } } },
  required: ['changed'],
}

const { task, acceptance = [], tests = [], id = '', ux = false, core = false, strong = false, resolved = null } = args
if (!task) throw new Error('build needs args.task')

// args.root runs the task in its own worktree, so tasks can build side by side.
const where = (args.root ? `Work only in the checkout at ${args.root}: cd there before anything else, run every command from it, and never read or change another checkout of this repository.\n\n` : '')
  + 'Never call the advisor tool, even if you have one: only the lead consults it.\n\n'
// A task names its domains; every agent reads those domains' files and Platform's.
const domainList = [args.domains ?? []].flat().map((d) => String(d).trim().toLowerCase()).filter(Boolean)
const domains = domainList.length
  ? `This task names its domains: ${domainList.join(', ')}. Read these rule files and no other domain's:\n${[...domainList, 'platform'].map((d) => `- docs/domains/${d}.md`).join('\n')}\n\n`
  : 'Read docs/domains/platform.md, and the file of each domain the task touches.\n\n'
const read = (name) => `${where}${domains}First read .claude/agents/${name}.md. It defines your role and its limits, and you must follow it exactly.\n\n`

const brief = `## Task${id ? ` ${id}` : ''}\n${task}\n\n## Acceptance criteria\n${acceptance.map((line) => `- ${line}`).join('\n')}\n\n## Test commands\n${tests.map((line) => `- ${line}`).join('\n')}`
// Told to every later agent, so a check the lead corrected is not reported or restored as tampering.
const resolvedNote = resolved ? `\n\nThe lead corrected these locked check files after a dispute, on purpose; work from the corrected version and never restore the original: ${resolved.files.join(', ')}. ${resolved.note}` : ''

// Model routing (docs/delivery.md, Models). Free models carry the mechanical work;
// Haiku carries judgement; a core task's Check and review, and everything
// that judges or builds a strong task, get stronger models.
const HAIKU = { model: 'claude-agent/haiku', effort: 'max' }
const SONNET = { model: 'claude-agent/sonnet', effort: 'high' }
const OPUS = { model: 'claude-agent/opus', effort: 'high' }
const MODELS = {
  'spec-writer': HAIKU,
  check: HAIKU,
  implementer: { model: 'opencode/beam-free', effort: 'high' },
  qa: { model: 'opencode/hy4-flash-free', effort: 'high' },
  'adversarial-reviewer': HAIKU,
  'ux-reviewer': HAIKU,
  helper: { model: 'opencode/mimo-v2.6-flash-free', effort: 'low' },
}
const CORE = { check: OPUS, 'adversarial-reviewer': SONNET }
const STRONG = { check: OPUS, implementer: OPUS, 'adversarial-reviewer': OPUS }
// args.models overrides a role's model for this run, as when the free models
// are out of allowance and Sonnet builds: { implementer: { model, effort } }.
const role = (name) => ({ agentType: 'general-purpose', ...MODELS[name], ...(core ? CORE[name] : {}), ...(strong ? STRONG[name] : {}), ...(args.models?.[name] ?? {}) })

let root = null
// note is for a snapshot whose prompt would repeat an earlier run's: on a
// resume, a repeated prompt replays the old tree from the cache.
const snapshot = async (label, phaseName, note = '') => {
  const result = await agent(`${where}Run \`bash tools/snapshot.sh\` from the repository root and return the printed tree hash, and the absolute path of the repository root (\`pwd\`). Do nothing else.${note}`,
    { label, phase: phaseName, schema: SNAPSHOT, ...MODELS.helper })
  if (!result) throw new Error(`snapshot failed: ${label}`)
  root = result.root
  return result.tree
}
// The lead checks a free model's file lists against git itself when a run stops on them.
const filesChanged = async (from, to, paths, label, phaseName) => {
  const only = paths.length ? ` -- ${paths.map((f) => `'${f}'`).join(' ')}` : ''
  const result = await agent(`${where}Run \`git diff --name-only ${from} ${to}${only}\` in the repository root and return the files it lists. Do nothing else.`,
    { label, phase: phaseName, schema: FILES, ...MODELS.helper })
  return result ? result.changed : null
}

const economy = 'Keep your context small: send command output to a file and read it with grep or tail, never whole. Report every blocker and major, and at most three minors. No style.'
const evidenceRule = 'A blocker or major needs evidence that runs: a failing test, a command with its output, or a measurement. A finding with only reading as evidence counts as minor.'
const listFindings = (list) => list.map((f) => `- [${f.severity}] ${f.title}: ${f.scenario}`).join('\n')

// ---- Spec: the criteria become checks before anything is built.
phase('Spec')
const base = await snapshot('snapshot base', 'Spec')
const specPrompt = (notes) => `${read('spec-writer')}${brief}${notes}\n\nWrite the acceptance checks for this task. Don't implement it and don't commit.`
let spec = await agent(specPrompt(''), { label: 'spec', phase: 'Spec', ...role('spec-writer'), schema: SPEC })
if (!spec) throw new Error('spec writer returned nothing')
let specTree = await snapshot('snapshot spec', 'Spec')

// ---- Check: one blind review of the design and the checks; revised once at most.
// A major sends the checks back too: a Check once named the case its checks
// missed, the build went ahead without it, and that case was the one it got wrong.
const checkList = (s) => s.checks.map((c, i) => `${i + 1}. ${c.criterion}\n   ${c.file}: ${c.name} (\`${c.command}\`)\n   Fails today: ${c.failsToday}\n   Wrong build it catches: ${c.wrongBuild}`).join('\n')
const checkPrompt = (s, tree) => `${read('adversarial-reviewer')}${brief}\n\n## Checks written for it\n${checkList(s)}\nCriteria the spec writer sharpened: ${s.criteriaChanges.join('; ') || 'none'}\nObserved: ${s.observed}\nUnchecked: ${s.unchecked.join('; ') || 'none'}${s.answers?.length ? `\nThe spec writer's answers to your first review:\n${s.answers.map((a) => `- ${a}`).join('\n')}` : ''}\n\nNothing is built yet: this is the review before a build. The checks are in \`git diff ${base} ${tree}\`. Never edit the working tree. ${evidenceRule} For a check, evidence can be a small wrong build or right build you made in your own export that the check judges wrongly. ${economy}`
let review = null
for (let attempt = 1; attempt <= 2; attempt++) {
  phase('Check')
  review = await agent(checkPrompt(spec, specTree), { label: `check ${attempt}`, phase: 'Check', ...role('check'), schema: FINDINGS })
  if (!review) throw new Error('check review returned nothing')
  const back = review.findings.filter((f) => f.severity !== 'minor')
  if (attempt === 2 || !back.length) {
    // resolved.settled names blockers the lead settled by correcting a check
    // (two locked requirements that contradict each other can't be answered by
    // the spec writer): they no longer stop the run.
    const settled = resolved?.settled ?? []
    const stops = back.filter((f) => f.severity === 'blocker' && !settled.some((t) => f.title.includes(t)))
    if (stops.length) return { clean: false, stoppedAt: 'Check', open: stops, spec, review: review.findings, base, specTree }
    break
  }
  phase('Spec')
  const notes = `\n\n## Review of your first checks\n${listFindings(back)}\n\nFor each finding, add or change checks so a build that ignores it fails, or answer why you disagree, with evidence, in answers. A design finding is answered with checks that pin the behaviour it asks for, not with code.`
  spec = await agent(specPrompt(notes), { label: 'spec revised', phase: 'Spec', ...role('spec-writer'), schema: SPEC })
  if (!spec) throw new Error('spec writer returned nothing')
  specTree = await snapshot('snapshot spec revised', 'Spec')
}
if (spec.criteriaChanges.length) log(`Criteria the spec writer sharpened (record them in the task's file): ${JSON.stringify(spec.criteriaChanges)}`)
const binding = review.findings.filter((f) => f.severity !== 'minor')
const reviewNotes = binding.length
  ? `\n\n## Review before the build\n${listFindings(binding)}\n\nEach finding above binds you: build what it asks, or answer in reviewAnswers why not, with evidence (a measurement, a failing approach, a conflicting check). The lead reads every answer; one without evidence counts as ignoring the review.`
  : ''
const settledNote = resolved?.settled?.length ? `\n\n## Settled by the lead\nThe lead settled the review's blockers named ${resolved.settled.map((t) => `"${t}"`).join(', ')} by correcting ${resolved.files.join(', ')}: ${resolved.note}` : ''

// The locked files are what the spec writer wrote. A file a check only cites,
// such as an existing script the task must change, stays open to the implementer.
const lockedFiles = await filesChanged(base, specTree, [], 'checks written', 'Spec')
if (!lockedFiles) throw new Error('could not list the files the spec writer wrote')
const locked = `\n\n## Locked checks\n${checkList(spec)}\n\nThese files are locked: ${lockedFiles.join(', ')}. Make the checks pass by building the task. Never edit, weaken, skip or delete a locked check, and never add an ignore file, a build tag or a skip to get past one. If you believe one is wrong, stop work on it and put it in disputes with your evidence.`

// lockBase is the tree the locked checks must stay as: the spec's, or the
// lead's correction of them after a dispute. resolved.unlock names files that
// were locked by accident and hold no check.
let lockBase = specTree
const unlocked = resolved?.unlock ?? []
const lockedChanged = async (head, label) => {
  if (!lockedFiles.length) return []
  const changed = await filesChanged(lockBase, head, lockedFiles, label, 'Build')
  return changed ? changed.filter((f) => !unlocked.includes(f)) : ['(could not check)']
}

// ---- Build.
phase('Build')
let built = await agent(`${read('implementer')}${brief}${reviewNotes}${settledNote}${locked}\n\nImplement this. Don't commit.`, { label: 'build', phase: 'Build', ...role('implementer'), schema: IMPLEMENTED })
if (!built) throw new Error('implementer returned nothing')
// For the lead only: the Verify reviewers stay blind to the implementer's reasoning.
if (built.reviewAnswers.length) log(`Implementer's answers to the review before the build: ${JSON.stringify(built.reviewAnswers)}`)
let head = await snapshot('snapshot build', 'Build')
let touched = await lockedChanged(head, 'locked checks')
// A dispute stops the run. The lead settles it from evidence; if the check was
// wrong, the lead corrects it and resumes with args.resolved = { files, note },
// so this run's Spec, Check and Build replay from cache.
if (resolved) {
  head = await snapshot('snapshot after the resolved dispute', 'Build', ` (This follows the lead's correction: ${resolved.note})`)
  touched = (await lockedChanged(head, 'locked checks after the resolved dispute')).filter((f) => !resolved.files.includes(f))
  lockBase = head
  // An implementer that stopped on its dispute left the build unfinished and
  // the checks unrun: it finishes now, on the corrected checks.
  if (built.disputes.length && !touched.length && !built.done) {
    const finished = await agent(`${read('implementer')}${brief}${reviewNotes}${locked}\n\n## Your dispute was settled\nYou stopped on a dispute. The lead settled it and corrected these files: ${resolved.files.join(', ')}. ${resolved.note}\n\n${unlocked.length ? `These files are not locked, whatever the list above says: ${unlocked.join(', ')}. ` : ''}Your partial build is in the working tree. Finish the task from it and run the checks and the whole of every package you changed. Don't commit.`, { label: 'build after the resolved dispute', phase: 'Build', ...role('implementer'), schema: IMPLEMENTED })
    if (!finished) throw new Error('implementer returned nothing')
    built = finished
    head = await snapshot('snapshot of the finished build', 'Build')
    touched = await lockedChanged(head, 'locked checks after the finished build')
  } else {
    built = { ...built, disputes: [], report: `${built.report}\n\nDispute resolved by the lead: ${resolved.note}` }
  }
}
if (touched.length || built.disputes.length) {
  return { clean: false, stoppedAt: 'Build', reason: touched.length ? 'locked checks changed' : 'checks disputed', touched, disputes: built.disputes, spec, base, specTree, head, build: built }
}

// ---- Verify: QA runs it, one blind reviewer tries to break it, UX when the task has ux.
let reviewersNote = ''
const setNote = () => { reviewersNote = `Someone else has built the task above. You are reviewing it, not doing it: never edit, stash or change the working tree. Inspect the change with \`git diff ${base} ${head}\` (the checks were written first, in \`git diff ${base} ${specTree}\`). To build or test, use your own export: \`git archive ${head} | tar -x -C <a directory of your own>\`, and run every command from inside it. Remove your export, any workdir and anything you started before you finish; remove containers with \`docker rm -fv\`.${resolvedNote}` }
const reviewers = (round) => {
  const list = [
    { key: 'qa', file: 'qa', focus: `${round === 1 ? 'Run what your role file lists, once, then try each acceptance criterion by hand.' : 'This round reviews fixes: rerun the whole of every package the fixes touch, and recheck the criteria they affect.'}\nHead tree: ${head}\nRecorded results: ${root}/.results/${head} (read-only; your role file says how to use them)` },
    { key: 'adversarial', file: 'adversarial-reviewer', focus: `This is the review after a build. Break it where the locked checks do not look, and say whether it is in the right place and what it costs.${core ? ' This task is core: it touches stored data, access, or the engine\'s jobs, leases or stores. Weigh the design, now that it is built, as carefully as the defects.' : ''}` },
  ]
  if (ux) list.push({ key: 'ux', file: 'ux-reviewer', focus: 'Review what was built, as a viewer or operator would meet it. Check the affected flow once, on the app, API or configuration a person would use.' })
  return list
}
// only: the reviewers to ask. Round 2 asks only those whose findings were fixed
// or who returned nothing, so a fix is not reviewed again by everyone.
const verify = async (round, only = null) => {
  phase('Verify')
  const asked = reviewers(round).filter((r) => !only || only.includes(r.key))
  const results = await parallel(asked.map((r) => () =>
    agent(`${read(r.file)}${brief}\n\n${reviewersNote}\n\n${r.focus}\n\n${evidenceRule} ${economy}`, { label: `${r.key} (round ${round})`, phase: 'Verify', ...role(r.file), schema: FINDINGS })
      .then((result) => ({ key: r.key, result }))))
  const answered = results.filter((x) => x && x.result)
  const missing = asked.map((r) => r.key).filter((k) => !answered.some((x) => x.key === k))
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

if (!open.length && result.missing.length) {
  // A reviewer that returned nothing is asked again; nothing needs fixing yet.
  const again = await verify(1, result.missing)
  history.push({ round: 1, ...again })
  result = { findings: [...result.findings, ...again.findings], missing: again.missing }
  open = blocking(result)
}

// ---- Fix: one round, then back to Spec if anything evidenced remains.
if (open.length) {
  phase('Fix')
  const list = open.map((f, i) => `${i + 1}. [${f.severity}] ${f.title} (${f.file ?? 'no file'})\nScenario: ${f.scenario}\nEvidence (${f.evidenceKind}): ${f.evidence}`).join('\n\n')
  const fixed = await agent(`${read('implementer')}${brief}${locked}${resolvedNote}\n\n## Evidenced findings\n${list}\n\nFor each real defect, find the cause that let it through, not only the case reported: if the design would let other cases of the same kind through, fix the design and test it with a second case of that kind. Add a test that catches it. If a finding is wrong, say why with evidence instead of changing code. Don't commit.`, { label: 'fix', phase: 'Fix', ...role('implementer'), schema: IMPLEMENTED })
  if (!fixed) throw new Error('fixer returned nothing')
  built = fixed
  head = await snapshot('snapshot fix', 'Fix')
  touched = await lockedChanged(head, 'locked checks after fix')
  if (touched.length || fixed.disputes.length) {
    return { clean: false, stoppedAt: 'Fix', reason: touched.length ? 'locked checks changed' : 'checks disputed', touched, disputes: fixed.disputes, spec, base, specTree, head, history }
  }
  setNote()
  const askAgain = [...new Set([...open.map((f) => f.reviewer), ...result.missing])]
  result = await verify(2, askAgain)
  history.push({ round: 2, ...result })
  open = blocking(result)
  log(`Verify 2: ${result.findings.length} finding(s), ${open.length} evidenced and blocking`)
}

const all = history.flatMap((h) => h.findings)
return {
  // Items the implementer left undone are the lead's to finish, never clean.
  clean: open.length === 0 && result.missing.length === 0 && !(built.unfinished ?? []).length,
  // Evidenced findings left after the one fix round mean the checks missed something.
  backToSpec: open.length > 0,
  id, base, specTree, head,
  open,
  unreviewed: result.missing,
  unfinished: built.unfinished ?? [],
  environment: all.filter((f) => f.kind === 'environment'),
  minors: all.filter((f) => f.severity === 'minor' && f.kind === 'defect'),
  criteriaChanges: spec.criteriaChanges,
  spec,
  review: review.findings,
  build: built,
  rounds: history.map((h) => ({ round: h.round, findings: h.findings.map((f) => ({ reviewer: f.reviewer, severity: f.severity, kind: f.kind, evidenceKind: f.evidenceKind, title: f.title })) })),
}
