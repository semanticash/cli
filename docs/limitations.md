# Limitations

Known constraints and intentional scope boundaries. Feature-specific caveats are documented in their respective pages - this is the cross-cutting summary.

---

## Platform support

- Official release targets are **macOS, Linux, and Windows** (amd64, arm64).
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
- **Provider metadata varies.** Claude Code provides line-level tool call content (Edit/Write payloads), enabling exact and formatted matching. Providers such as Cursor, Kiro IDE, and Kiro CLI may only report file-level tool metadata, which limits attribution to hunk-overlap matching.
- Manual edits after AI generation downgrade matches from "exact" to "modified." Mixed human/AI edits in the same hunk are attributed as modified rather than exact.
- Carry-forward is per-file, not per-line across windows. If the same file has current-window AI activity, Semantica keeps that file current-window authoritative instead of merging historical and current AI lines inside one file.
- Attribution is computed against the diff between checkpoints. Squashed or rebased commits that collapse multiple checkpoints may produce less precise results.

## Playbooks and suggestions

- Require at least one supported LLM CLI installed and authenticated: Claude Code (`claude`), Cursor CLI (`agent`), Gemini CLI (`gemini`), or Copilot CLI (`copilot`).
- Playbook generation uses bounded diff input to stay within LLM context limits. Commit message and PR suggestions use structured change summaries plus selected per-file excerpts. Large diffs may still produce less precise summaries.
- `semantica suggest pr` uses the committed branch diff against the base ref. Uncommitted working-tree changes are not included in the suggestion.
- `semantica suggest pr` detects the base branch best-effort. Repos with non-standard default branch names may need `--base` explicitly.
- Playbook generation is asynchronous - results are not immediately available after `--generate`.

## Kiro IDE

- Kiro IDE hooks do not expose an explicit session ID to external commands. Semantica pairs `promptSubmit` and `agentStop` by workspace-scoped capture state and chooses the session best-effort at prompt submission.
- If multiple Kiro chats exist for the same workspace, Semantica may still select the wrong one at prompt submission because the hook API does not identify the active chat directly.

## Kiro CLI

- Kiro CLI support currently uses a dedicated repo-local agent config at `.kiro/agents/semantica.json`. Semantica capture is active only when the current Kiro CLI session is using that config. You can select it with `kiro-cli chat --agent semantica`, or make it the repo default with `kiro-cli agent set-default semantica`.
- Kiro CLI hook payloads do not expose a conversation ID directly. Semantica pairs `userPromptSubmit` and `stop` by workspace-scoped capture state and resolves the active conversation best-effort from the current workspace.
- If `userPromptSubmit` is missed for a turn, the following `stop` event cannot reconstruct the missing boundary for that turn.

## Hosted reporting

- Hosted features require CLI authentication plus a repo connection via `semantica connect`.
- Additional remote setup may be required depending on where you want attribution to appear.
- Hosted sync is best-effort with a 10-second timeout. Failures never block the worker, the commit, or any local feature.

## Secret redaction

- Secret redaction is outbound only. Local raw capture, transcript payloads, and blob content in `.semantica/` remain unchanged.
- Detection is best-effort and uses embedded Gitleaks rules. Unknown formats may be missed, and false positives can still remove some prompt context.
- Path normalization covers the provenance fields Semantica knows how to rewrite today. New or provider-specific fields may require follow-up support.
- Redaction lowers the chance of leaking credentials or local filesystem details, but it does not guarantee that synced prompts, command output, or edited content are free of sensitive business context.
