<!-- Not a symlink: Claude Code reads CLAUDE.md in place of AGENTS.md when both exist, so this imports AGENTS.md (the one source every agent shares) and adds only what is true of Claude Code alone. Keep it short. -->
@AGENTS.md

## Claude Code

- Subagent roles are the files in `.claude/agents/`. The `build` workflow is `.claude/workflows/build.js`, started with the Workflow tool.
- Only the lead calls the advisor tool, and only when stuck. Every other agent has the tool and must not use it.
- Model names in `docs/delivery.md` and the role files are written for OpenCode (`provider/model`). In Claude Code alone, `claude-agent/opus`, `claude-agent/sonnet` and `claude-agent/haiku` are `opus`, `sonnet` and `haiku`, and Haiku stands in where a free model is named.
