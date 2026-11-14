export const meta = {
  name: 'build',
  description: 'Write the acceptance checks first, build until they pass, then verify by running it and one blind adversarial review',
  whenToUse: 'Every task from tasks/prd.json. Args: {task, acceptance: string[], tests: string[], id?: string, ux?: boolean, core?: boolean}',
  phases: [
    { title: 'Spec', detail: 'acceptance criteria become executable checks that fail today' },
    { title: 'Check', detail: 'review the design and whether the checks can catch a wrong build' },
    { title: 'Build', detail: 'one implementer, until the checks pass; checks are locked' },
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
    answers: { type: 'array', items: { type: 'string' }, description: 'For a revision after review: each finding, and the check that now covers it, or why you disagree, with evidence' },
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
    designAnswers: { type: 'array', items: { type: 'string' }, description: 'Each blocker or major of the design review: what you built for it, or why not, with evidence' },
  },
  required: ['done', 'report', 'disputes', 'unfinished', 'designAnswers'],
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


const { task, tests = [], id = '', ux = false, core = false, resolved = null, refine = true } = args
let { acceptance = [] } = args
// Told to every later reviewer and fixer, so a corrected check is not reported or restored as tampering.
const resolvedNote = resolved ? `\n\nThe lead corrected these locked check files after a dispute, on purpose; review the corrected version and never restore the original: ${resolved.files.join(', ')}. ${resolved.note}` : ''
if (!task) throw new Error('build needs args.task')

// ---- Refine: thin criteria predicted the tasks that went back to Spec (JS-027,
// JS-028: two criteria each, five attempts between them), so before any check is
// written the strongest model sharpens them against the code. It may only make a
// criterion more exact; the lead reads the changes it logs and records them in
// tasks/prd.json. A later attempt passes the refined list with refine: false.
const REFINED = {
  type: 'object',
  properties: {
    acceptance: { type: 'array', items: { type: 'string' }, description: 'The refined criteria, in order' },
    changes: { type: 'array', items: { type: 'string' }, description: 'Each change and why, citing the code or doc that prompted it' },
  },
  required: ['acceptance', 'changes'],
}
if (refine) {
  const refined = await agent(`You sharpen the acceptance criteria of one jellyscale task before its checks are written. Read PRODUCT.md, the parts of DOMAINS.md and ARCHITECTURE.md the task touches, and the code it will change (targeted reads, not whole trees). Then rewrite the criteria so a spec writer can turn each into a check that fails on a plausible wrong build:\n- Name the cases a wrong build would get wrong: counts and bounds (how many reads, runs, jobs; "at most", "each"), failures that are the file's and failures that are not (store, timeout, signal, lost lease), several of a thing as well as one, a replaced or changed file, two replicas or workers at once, and what the viewer or operator sees.\n- Split a criterion that holds several claims; state an "always" or "whatever X does" claim only with the bound it really has.\n- Keep every guarantee the criteria give: never drop, weaken or narrow one, and never add a feature the task did not ask for. If a criterion seems wrong, keep it and say why in changes.\n- Keep the criteria few and plain: one line each, no implementation.\nDo not edit any file, and do not run tests, Docker or servers.\n\n## Task${id ? ` ${id}` : ''}\n${task}\n\n## Acceptance criteria\n${acceptance.map((line) => `- ${line}`).join('\n')}`,
    { label: 'refine criteria', phase: 'Spec', model: 'claude-fable-5-2', effort: 'high', schema: REFINED })
  if (refined && refined.acceptance.length) {
    acceptance = refined.acceptance
    log(`Refined criteria (record in tasks/prd.json): ${JSON.stringify(refined)}`)
  } else {
    log('Criteria refinement returned nothing; building on the criteria as given')
  }
}

const criteria = acceptance.map((line) => `- ${line}`).join('\n')
const commands = tests.map((line) => `- ${line}`).join('\n')
const brief = `## Task${id ? ` ${id}` : ''}\n${task}\n\n## Acceptance criteria\n${criteria}\n\n## Test commands\n${commands}`

const MODELS = {
  'spec-writer': { model: 'sonnet' },
  implementer: { model: 'sonnet' },
  'architecture-reviewer': { model: 'sonnet' },
  // Check reads only the spec and its checks, and a gap it misses costs a
  // whole build and Verify (about 1M tokens), so it gets the strongest model.
  // Pinned by ID: the `fable` alias follows the CLI version of the session.
  check: { model: 'claude-fable-5-2' },
  'adversarial-reviewer': { model: 'sonnet' },
  qa: { model: 'haiku', effort: 'high' },
  'ux-reviewer': { model: 'haiku', effort: 'high' },
}
// Roles are read from their files, so a role added in this session works too.
const role = (name) => ({ agentType: 'general-purpose', ...MODELS[name] })
const read = (name) => `First read .claude/agents/${name}.md. It defines your role and its limits, and you must follow it exactly.\n\n`

let root = null
// note is for a snapshot whose prompt would repeat an earlier run's: on a resume
// a repeated prompt replays the old tree from the cache.
const snapshot = async (label, phase, note = '') => {
  const result = await agent(`Run \`bash tools/snapshot.sh\` from the repository root and return the printed tree hash, and the absolute path of the repository root (\`pwd\`). Do nothing else.${note}`,
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
const checkPrompt = (s, tree) => `${read('architecture-reviewer')}${brief}\n\n## Checks written for it\n${checkList(s)}\nObserved: ${s.observed}\nUnchecked: ${s.unchecked.join('; ') || 'none'}${s.answers?.length ? `\nThe spec writer's answers to your first review:\n${s.answers.map((a) => `- ${a}`).join('\n')}` : ''}\n\nNothing is built yet; the checks are in \`git diff ${base} ${tree}\`. Review two things: the design (where this work should live and in what shape, against ARCHITECTURE.md and PRODUCT.md), and the checks: would each fail on a plausible wrong build, do they cover the criteria, was real behaviour observed where a protocol is involved? Never edit the working tree. ${evidenceRule}`
let design = null
for (let attempt = 1; attempt <= 2; attempt++) {
  phase('Check')
  design = await agent(checkPrompt(spec, specTree), { label: `check ${attempt}`, phase: 'Check', ...role('check'), schema: FINDINGS })
  if (!design) throw new Error('check review returned nothing')
  // A major sends the checks back once too: JS-071's Check named the case its
  // checks missed and a better design as majors, the build went ahead without
  // either, and the case it named was the one the build got wrong. A second
  // review stops the run only on a blocker; its majors bind the implementer.
  const back = design.findings.filter((f) => f.severity !== 'minor')
  if (attempt === 2 || !back.length) {
    const stops = back.filter((f) => f.severity === 'blocker')
    if (stops.length) return { clean: false, stoppedAt: 'Check', open: stops, spec, design, base, specTree }
    break
  }
  phase('Spec')
  const notes = `\n\n## Review of your first checks\n${back.map((f) => `- [${f.severity}] ${f.title}: ${f.scenario}`).join('\n')}\n\nFor each finding, add or change checks so a build that ignores it fails, or answer why you disagree, with evidence, in answers. A design finding is answered with checks that pin the behaviour it asks for, not with code.`
  spec = await agent(specPrompt(notes), { label: 'spec revised', phase: 'Spec', ...role('spec-writer'), schema: SPEC })
  if (!spec) throw new Error('spec writer returned nothing')
  specTree = await snapshot('snapshot spec revised', 'Spec')
}
const designNotes = design.findings.length
  ? `\n\n## Design review\n${design.findings.map((f) => `- [${f.severity}] ${f.title}: ${f.scenario}`).join('\n')}\n\nEach blocker and major above binds you: build what it asks, or answer in designAnswers why not, with evidence (a measurement, a failing approach, a conflicting check). The lead reads every answer; one without evidence counts as ignoring the review.`
  : ''
// The checks are what the spec writer wrote. A file a check only cites, such as an
// existing script the task must change, stays open to the implementer.
const written = await agent(`Run \`git diff --name-only ${base} ${specTree}\` in the repository root and return the files it lists. Do nothing else.`,
  { label: 'checks written', phase: 'Spec', schema: LOCKED, model: 'haiku', effort: 'low' })
if (!written) throw new Error('could not list the files the spec writer wrote')
const lockedFiles = written.changed
const locked = `\n\n## Locked checks\n${checkList(spec)}\n\nThese files are locked: ${lockedFiles.join(', ')}. Make the checks pass by building the task. Never edit, weaken, skip or delete a locked check. If you believe one is wrong, stop work on it and put it in disputes with your evidence.`

// lockBase is the tree the locked checks must stay as: the spec's, or the
// lead's correction of them after a dispute.
let lockBase = specTree
// resolved.unlock names files the lead found locked by mistake (in the spec's
// tree only by accident): the implementer may change them.
const unlocked = resolved?.unlock ?? []
const lockedChanged = async (head, label) => {
  const result = await agent(`Run \`git diff --name-only ${lockBase} ${head} -- ${lockedFiles.map((f) => `'${f}'`).join(' ')}\` in the repository root and return the files it lists. Do nothing else.`,
    { label, phase: 'Build', schema: LOCKED, model: 'haiku', effort: 'low' })
  return result ? result.changed.filter((f) => !unlocked.includes(f)) : ['(could not check)']
}

// ---- Build.
phase('Build')
let built = await agent(`${read('implementer')}${brief}${designNotes}${locked}\n\nImplement this. Don't commit.`, { label: 'build', phase: 'Build', ...role('implementer'), schema: IMPLEMENTED })
if (!built) throw new Error('implementer returned nothing')
// For the lead only: the Verify reviewers stay blind to the implementer's reasoning.
if (built.designAnswers.length) log(`Implementer's answers to the design review: ${JSON.stringify(built.designAnswers)}`)
let head = await snapshot('snapshot build', 'Build')
let touched = await lockedChanged(head, 'locked checks')
// A dispute stops the run. The lead settles it from evidence; if the check was
// wrong, the lead corrects it and resumes with args.resolved = { files, note },
// so this run's Spec, Check and Build replay from cache. resolved.light says
// every reviewer has already seen this build and the correction leaves its
// design as it was: then only QA and the adversarial reviewer look again.
if (resolved) {
  head = await snapshot('snapshot after the resolved dispute', 'Build', ` (This follows the lead's correction: ${resolved.note})`)
  touched = (await lockedChanged(head, 'locked checks after the resolved dispute')).filter((f) => !resolved.files.includes(f))
  lockBase = head
  // An implementer that stopped on its dispute left the build unfinished and the
  // checks unrun: it finishes now, on the corrected checks. (JS-025 skipped this,
  // and a stale test reached the merge queue.)
  if (built.disputes.length && !touched.length) {
    const finished = await agent(`${read('implementer')}${brief}${designNotes}${locked}\n\n## Your dispute was settled\nYou stopped on a dispute. The lead settled it and corrected these files: ${resolved.files.join(', ')}. ${resolved.note}\n\n${unlocked.length ? `These files are not locked, whatever the list above says: ${unlocked.join(', ')}. ` : ''}Your partial build is in the working tree. Finish the task from it and run the checks and the tests of the packages you changed. Don't commit.`, { label: 'build after the resolved dispute', phase: 'Build', ...role('implementer'), schema: IMPLEMENTED })
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

// What every reviewer is told: the change to read and how to stay out of its tree.
let reviewersNote = ''
const setNote = () => { reviewersNote = `Someone else has built the task above. You are reviewing it, not doing it: never edit, stash or mutate the working tree. Inspect the change with \`git diff ${base} ${head}\` (the checks were written first, in \`git diff ${base} ${specTree}\`). To build or test, use your own export: \`git archive ${head} | tar -x -C <dir in your scratchpad>\`, plus a symlink to web-dist for e2e, and run every command from inside it. Remove your export, any workdir and anything you started before you finish (/tmp is RAM with a per-user quota; put large workdirs under ~/.cache/jellyscale/agents/); remove containers with \`docker rm -fv\`.${resolvedNote}` }

// ---- Verify: QA runs it, one adversarial reads to break it, architecture only for core changes.
const reviewers = (round) => {
  const list = [
    { key: 'qa', file: 'qa', focus: `${round === 1 ? 'Run the checks and the tests of the packages the change touches, once, and check each acceptance criterion, including paths the checks skip. Do not run whole tiers, repeat runs to hunt flakes, or rerun under ffmpeg 6.1: the merge queue runs every tier on CI, and a failure there is fixed then.' : 'This round reviews fixes: rerun what the fixes touch and recheck the criteria they affect.'}\nHead tree: ${head}\nRecorded results: ${root}/.results/${head} (read-only; your role file says how to use them)` },
    { key: 'adversarial', file: 'adversarial-reviewer', focus: 'Break it where the locked checks do not look: concurrency across replicas and workers, partial failure, disconnects, leaks, client input, security, and whether the checks can really fail.' },
  ]
  if (resolved?.light) return list
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
  const findings = [...answered.flatMap((x) => x.result.findings.map((f) => ({ ...f, reviewer: x.key })))]
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
  const fixed = await agent(`${read('implementer')}${brief}${locked}${resolvedNote}\n\n## Evidenced findings\n${list}\n\nFor each real defect, find the cause that let it through, not only the case reported: if a design would let other cases of the same kind through, fix the design and test it with a second case of that kind, not just the one reported. Add a test that catches it. If a finding is wrong, say why with evidence rather than changing code. Don't commit.`, { label: 'fix', phase: 'Fix', ...role('implementer'), schema: IMPLEMENTED })
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
