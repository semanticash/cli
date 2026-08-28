# Limitations

Known constraints and intentional scope boundaries. Feature-specific caveats are documented with each feature. See the [Evidence Contract](evidence-contract.md) for attribution-specific limits.

---

## Platform support

- Official release targets are **macOS, Linux, and Windows** (amd64, arm64).
- `semantica launcher` is optional. It supports macOS (launchd), Linux (systemd user instance), and Windows (Task Scheduler). Other platforms use the default detached worker path.
- Windows support requires Git for Windows, which provides the POSIX shell used by Git hooks.
- Clipboard support for `semantica suggest commit` and `semantica suggest pr --copy` requires `pbcopy` (macOS), `wl-copy`/`xclip`/`xsel` (Linux), or `clip` (Windows). The commands still work without clipboard support - they print to stdout.

## Capture scope

- Capture only happens where Semantica hooks are installed. In practice, sessions launched from a Semantica-enabled repo are captured; activity in repos without `semantica enable` is not.
- Capture is **per-machine**. Another developer or CI runner working on the same repo needs its own Semantica setup to capture their sessions.
- If the CLI is upgraded or the capture state directory (`$SEMANTICA_HOME/capture/`) is cleared mid-session, offset state for in-progress sessions is lost. The worker reconciliation pass recovers what it can, but some events may be missed.
- Repository workers reconcile only capture state owned by the locked repository. Cross-repository sessions wait for an unscoped capture. Transcript switches preserve unresolved segments as doctor-visible orphan snapshots, but do not replay them automatically.

## Git and repo boundaries

- Commit manifests contain the linked commit's tracked Git tree; untracked and ignored files are excluded. Workspace manifests, including manual and baseline checkpoints, also include untracked, non-ignored files.
- Nested repositories are treated as separate ownership scopes - events are routed to the deepest matching repo root.
- Commit-linked processing is serialized per repository. Transient failures retry with bounded backoff. A terminally failed queue head blocks later records until it is repaired and retried; `semantica doctor` reports the blocking record.
- Without the optional launcher, retries due after the current worker exits wait for the next commit or manual drain. Launcher installations add a 30-minute recovery interval.

## Attribution fidelity

- Historical carry-forward applies to created files present in the previous commit-linked manifest and to modified files whose committed content matches an earlier unlinked workspace observation created by `semantica checkpoint`. Modified-file evidence is limited to that observation's event window and does not replace current-window line attribution. Missing or mismatched observations fail closed.
- Automatic workspace observations are not supported. The internal `workspace_freeze` experiment does not materialize observations and must not be enabled in production repositories.
- **Provider metadata varies.** Claude Code, Kiro CLI, and Kiro IDE provide line-level file-edit content for supported edit actions, enabling exact and formatted matching. Providers such as Cursor may only report file-level tool metadata. Those weaker provider-touch signals are preserved as evidence and `ai_provider_only_lines`, but excluded from the headline AI percentage instead of being treated as equivalent to line-level matches.
- **Shell attribution scores verified deltas only.** Claude Code and Codex Bash hooks capture bounded workspace deltas. Scoring is on by default; opt out with `attribution_v2: false` or `SEMANTICA_ATTRIBUTION_V2=0`. Only verified single-actor deltas are scored. Partial and ambiguous deltas are excluded; binary, truncated, symlink, gitlink, and unaligned changes retain file-level evidence only. Other shell providers contribute command provenance and recognized deletion evidence unless they also emit file-edit hooks.
- **Tool-delta evidence is time-bounded, not exclusive.** A delta shows that changed lines appeared while an agent-issued tool was running. Concurrent saves, formatters, and file watchers can produce the same evidence.
- **Capture and scoring are separate.** Semantica snapshots eligible Bash calls when capture state is active, even if `attribution_v2` is disabled. The flag controls scoring, not capture. Calls without capture state are ignored. On-demand or recomputed attribution can use earlier captures; enabling the flag alone does not update stored results.
- Commit-message attribution remains on v1 to keep the synchronous hook bounded. Background enrichment, `semantica blame`, and hosted attribution use the repository's selected version.
- Manual edits after direct AI generation may downgrade matches from "exact" to "modified." Tool-delta lines are attributed only when exact or whitespace-normalized content survives in the commit; unmatched later edits remain human.
- Carry-forward is per-file. Current-window attribution remains authoritative when an eligible created file already has AI evidence.
- Attribution is computed against the diff between commit lineage records. Squashed or rebased commits that collapse multiple records may produce less precise results.

## Playbooks and suggestions

- Require at least one supported LLM CLI installed and authenticated: Claude Code (`claude`), Codex (`codex`), Cursor CLI (`agent`), Gemini CLI (`gemini`), Copilot CLI (`copilot`), or Kiro CLI (`kiro-cli`). For Claude Code, the binary bundled inside the VS Code extension is also discovered automatically when the standalone CLI is not on PATH.
- Playbook generation uses bounded diff input to stay within LLM context limits. Commit message and PR suggestions use structured change summaries plus selected per-file excerpts. Large diffs may still produce less precise summaries.
- `semantica suggest pr` uses the committed branch diff against the base ref. Uncommitted working-tree changes are not included in the suggestion.
- `semantica suggest pr` detects the base branch best-effort. Repos with non-standard default branch names may need `--base` explicitly.
- Playbook generation is asynchronous - results are not immediately available after `--generate`.

## Kiro IDE

- Kiro IDE hooks do not expose an explicit session ID to external commands. Semantica pairs `promptSubmit`, `fileEdited`, and `agentStop` by workspace-scoped capture state and chooses the session best-effort at prompt submission.
- If multiple Kiro chats exist for the same workspace, Semantica may still select the wrong one at prompt submission because the hook API does not identify the active chat directly.
- Kiro IDE rename actions are file-touch attribution only because Kiro does not provide old/new content for `smartRelocate`.

## OpenAI Codex

- Codex project hooks require approval through `/hooks` in the CLI or Settings > Hooks in the desktop app.
- Legacy user-global Semantica hooks remain installed to protect other repositories. Remove them after migrating all repositories; duplicate delivery is deduplicated but adds latency and lock contention.
- Codex captures only when the hook payload `cwd` belongs to an enabled repository.
- Codex rollout/session files are not replayed. Hook payloads are the capture source.
- Codex subagent activity is not captured because Codex does not expose a stable child-agent and session-linking surface.

## Kiro CLI

- Kiro CLI uses a dedicated repo-local agent config at `.kiro/agents/semantica.json`. Semantica capture is active only when the Kiro CLI session uses that config. Select it with `kiro-cli chat --agent semantica`, or make it the repo default with `kiro-cli agent set-default semantica`.
- Kiro CLI hook payloads do not expose a conversation ID directly. Semantica pairs `userPromptSubmit` and `stop` by workspace-scoped capture state and resolves the active conversation best-effort from the current workspace.
- Direct `postToolUse` hooks own parent Kiro CLI file and shell capture. Parent SQLite transcript replay is disabled to avoid duplicate events with mismatched provider tool IDs.
- AgentCrew child sessions are replayed from Kiro JSONL files only when discovery has a single parent-shaped session anchor in the same cwd and prompt-to-stop window. Overlapping same-repo Kiro sessions or missing parent metadata cause child replay to fail closed.
- If `userPromptSubmit` is missed for a turn, later tool hooks may not have capture state to attach to for that turn.
- Playbook generation and other LLM-backed features try Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot CLI, and Kiro CLI in that order. Kiro CLI generation uses headless mode (`kiro-cli chat --no-interactive`) and requires Kiro CLI to have a valid cached login or `KIRO_API_KEY`.

## Hosted reporting

- Hosted features require CLI authentication plus a repo connection via `semantica connect`.
- Additional remote setup may be required depending on where you want attribution to appear.
- Hosted sync is best-effort with a 10-second timeout. Failures never block the worker, the commit, or any local feature.
- Historical checkpoints created before readiness markers may report attribution or sync as unknown even when work previously completed.

## Secret redaction

- Secret redaction is outbound only. Local raw capture, transcript payloads, and blob content in `.semantica/` remain unchanged.
- Detection is best-effort and uses embedded Gitleaks rules. Unknown formats may be missed, and false positives can still remove some prompt context.
- If outbound redaction cannot complete for a sync artifact, Semantica fails that upload closed instead of sending the raw artifact.
- Path normalization covers recognized provenance fields. Unrecognized provider-specific fields may remain unchanged.
- Redaction lowers the chance of leaking credentials or local filesystem details, but it does not guarantee that synced prompts, command output, or edited content are free of sensitive business context.
