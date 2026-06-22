# Limitations

Known constraints and intentional scope boundaries. Feature-specific caveats are documented in their respective pages - this is the cross-cutting summary.

---

## Platform support

- Official release targets are **macOS, Linux, and Windows** (amd64, arm64).
- `semantica launcher` is still experimental. It currently supports macOS (launchd), Linux (systemd user instance), and Windows (Task Scheduler). Other platforms keep using the default detached worker path.
- Windows support requires Git for Windows, which provides the POSIX shell used by Git hooks.
- Clipboard support for `semantica suggest commit` and `semantica suggest pr --copy` requires `pbcopy` (macOS), `wl-copy`/`xclip`/`xsel` (Linux), or `clip` (Windows). The commands still work without clipboard support - they print to stdout.
- On Windows, `semantica rewind` cannot restore symlinks without Developer Mode enabled or administrator privileges.

## Capture scope

- Capture only happens where Semantica hooks are installed. In practice, sessions launched from a Semantica-enabled repo are captured; activity in repos without `semantica enable` is not.
- Capture is **per-machine**. Another developer or CI runner working on the same repo needs its own Semantica setup to capture their sessions.
- If the CLI is upgraded or the capture state directory (`$SEMANTICA_HOME/capture/`) is cleared mid-session, offset state for in-progress sessions is lost. The worker reconciliation pass recovers what it can, but some events may be missed.

## Git and repo boundaries

- **Rewind only affects the working tree.** It does not rewrite Git history, modify the index, or unstage changes. Files are restored on disk only.
- Checkpoint manifests include git-tracked files and untracked, non-ignored files. Ignored files are not captured or restored.
- Nested repositories are treated as separate ownership scopes - events are routed to the deepest matching repo root.

## Attribution fidelity

- Attribution is anchored to captured session data within the checkpoint delta window. Deferred created files can carry forward AI attribution from earlier history when they were already present in the previous commit-linked manifest but committed later.
- **Provider metadata varies.** Claude Code, Kiro CLI, and Kiro IDE provide line-level file-edit content for supported edit actions, enabling exact and formatted matching. Providers such as Cursor may only report file-level tool metadata. Those weaker provider-touch signals are preserved as evidence and `ai_provider_only_lines`, but excluded from the headline AI percentage instead of being treated as equivalent to line-level matches.
- **Shell side effects are limited.** Bash and shell-tool events are captured as command provenance, but Semantica currently extracts file-touch evidence only for recognized deletion commands such as `rm`. Shell writes such as `echo > file`, `tee`, or `cp` do not synthesize file-touch or line-level attribution unless the provider also emits a file-edit hook for the affected file.
- Manual edits after AI generation downgrade matches from "exact" to "modified." Mixed human/AI edits in the same hunk are attributed as modified rather than exact.
- Carry-forward is per-file, not per-line across windows. If the same file has current-window AI activity, Semantica keeps that file current-window authoritative instead of merging historical and current AI lines inside one file.
- Attribution is computed against the diff between checkpoints. Squashed or rebased commits that collapse multiple checkpoints may produce less precise results.

## Playbooks and suggestions

- Require at least one supported LLM CLI installed and authenticated: Claude Code (`claude`), Cursor CLI (`agent`), Gemini CLI (`gemini`), or Copilot CLI (`copilot`). For Claude Code, the binary bundled inside the VS Code extension is also discovered automatically when the standalone CLI is not on PATH.
- Playbook generation uses bounded diff input to stay within LLM context limits. Commit message and PR suggestions use structured change summaries plus selected per-file excerpts. Large diffs may still produce less precise summaries.
- `semantica suggest pr` uses the committed branch diff against the base ref. Uncommitted working-tree changes are not included in the suggestion.
- `semantica suggest pr` detects the base branch best-effort. Repos with non-standard default branch names may need `--base` explicitly.
- Playbook generation is asynchronous - results are not immediately available after `--generate`.

## Intent-gap analysis

- Intent-gap analysis is opt-in and requires an authenticated, connected
  repository. It is not part of offline core capture.
- Analysis is manual: it runs only when the user invokes
  `semantica intent-gap analyze` or the `semantica-intent-gap` agent skill.
  The CLI requires an open pull request already known to Semantica for the
  current branch.
- The analyzed range contains committed changes between the merge base and
  `HEAD`. Staged and uncommitted changes are excluded.
- Prompt evidence is limited to captured user prompts linked to commits in that
  range. Missing or incomplete capture reduces the available intent signal. No
  captured prompts is an insufficient-evidence outcome, not proof that no gap
  exists.
- The hosted check currently uses the same empty-findings label for completed
  analysis and the no-captured-prompts path. Treat `no gaps found` as advisory;
  it does not confirm that prompt coverage was available.
- Analyzer input is capped at 96 KiB of cumulative diff, 100 commits, and 200
  captured prompts. When prompt history exceeds the cap, the oldest prompts are
  dropped first.
- Citation filtering verifies captured turn IDs, redacted prompt excerpts and
  hashes, and whether cited file ranges intersect the cumulative diff. It does
  not currently verify `evidence_class` or `ai_authored_regions_checked`
  against line-level attribution data.
- `deferred` findings are model judgments in this version. The CLI does not yet
  reconstruct a per-commit add/remove trajectory to verify deferral
  deterministically.
- Findings are advisory and may be incomplete or incorrect. JSON Schema,
  canonical IDs, and citation filtering reduce malformed or fabricated output
  but do not eliminate LLM error.

## Kiro IDE

- Kiro IDE hooks do not expose an explicit session ID to external commands. Semantica pairs `promptSubmit`, `fileEdited`, and `agentStop` by workspace-scoped capture state and chooses the session best-effort at prompt submission.
- If multiple Kiro chats exist for the same workspace, Semantica may still select the wrong one at prompt submission because the hook API does not identify the active chat directly.
- Kiro IDE rename actions are file-touch attribution only because Kiro does not provide old/new content for `smartRelocate`.

## OpenAI Codex

- Codex hooks are user-global. Semantica captures only when the hook payload `cwd` resolves to a Semantica-enabled repo; sessions outside enabled repos exit silently and record nothing.
- Codex rollout/session files are not replayed today. Hook payloads are the capture source.
- Codex subagent capture is deferred until Codex exposes a stable child-agent feature and session-linking surface.

## Kiro CLI

- Kiro CLI support currently uses a dedicated repo-local agent config at `.kiro/agents/semantica.json`. Semantica capture is active only when the current Kiro CLI session is using that config. You can select it with `kiro-cli chat --agent semantica`, or make it the repo default with `kiro-cli agent set-default semantica`.
- Kiro CLI hook payloads do not expose a conversation ID directly. Semantica pairs `userPromptSubmit` and `stop` by workspace-scoped capture state and resolves the active conversation best-effort from the current workspace.
- Direct `postToolUse` hooks own parent Kiro CLI file and shell capture. Parent SQLite transcript replay is disabled to avoid duplicate events with mismatched provider tool IDs.
- AgentCrew child sessions are replayed from Kiro JSONL files only when discovery has a single parent-shaped session anchor in the same cwd and prompt-to-stop window. Overlapping same-repo Kiro sessions or missing parent metadata cause child replay to fail closed.
- If `userPromptSubmit` is missed for a turn, later tool hooks may not have capture state to attach to for that turn.
- Playbook generation and other LLM-backed features try Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot CLI, and Kiro CLI in that order. Kiro CLI generation uses headless mode (`kiro-cli chat --no-interactive`) and requires Kiro CLI to have a valid cached login or `KIRO_API_KEY`.

## Hosted reporting

- Hosted features require CLI authentication plus a repo connection via `semantica connect`.
- Additional remote setup may be required depending on where you want attribution to appear.
- Hosted sync is best-effort with a 10-second timeout. Failures never block the worker, the commit, or any local feature.

## Secret redaction

- Secret redaction is outbound only. Local raw capture, transcript payloads, and blob content in `.semantica/` remain unchanged.
- Detection is best-effort and uses embedded Gitleaks rules. Unknown formats may be missed, and false positives can still remove some prompt context.
- If outbound redaction cannot complete for a sync artifact, Semantica fails that upload closed instead of sending the raw artifact.
- Path normalization covers the provenance fields Semantica knows how to rewrite today. New or provider-specific fields may require follow-up support.
- Redaction lowers the chance of leaking credentials or local filesystem details, but it does not guarantee that synced prompts, command output, or edited content are free of sensitive business context.
