# Evidence Contract

This document defines Semantica's attribution evidence classes and their limits.
Evidence comes from captured prompts, tool events, edits, and workspace deltas.
It does not establish exclusive authorship or model reasoning.

## The headline number

The headline AI percentage includes line-level evidence only:

```
ai_percentage = (exact + formatted + modified) / total_added_lines * 100
```

`ai_lines = exact + formatted + modified`. `ai_provider_only_lines` is a
separate file-level signal and is not included in `ai_lines` or
`ai_percentage`.

Deleted non-blank lines are recorded per file but excluded from
`total_added_lines`. Deletion-only edits and deleted files remain in changed-file
totals.

## Evidence classes

Each file has one primary evidence class. The three line-level classes contribute
to the headline percentage. The five fallback classes do not.

| Class | Produced from | Tier | Supports | Does not establish |
|-------|---------------|------|----------|--------------------|
| `exact` | Added line matches captured AI output character-for-character after trimming | line-level | the line's content matches captured AI output (trimmed) | that AI is the sole author, or that a human did not also produce identical content |
| `normalized` | Match after stripping all whitespace (catches formatter/linter reflow) | line-level | the line matches captured AI output modulo whitespace | byte-exact identity |
| `modified` | Line sits in a diff hunk overlapping captured AI output, without a line match | line-level | the line is in a hunk that overlaps captured AI output | that the final line's content is AI output |
| `tool_delta_touch` | Verified tool-window delta with no surviving line match | fallback | a file change was captured while an agent tool ran | which lines changed or which process produced them |
| `provider_touch` | Explicit file-edit tool event from a provider, no line-level payload | fallback | the provider reported a file-edit event for this file | which lines, or that the edit survived to the commit |
| `provider_coarse` | Session-level linkage only (no direct file-edit event) | fallback | an AI session was active in this file's window | that the provider edited this specific file |
| `carry_forward` | Prior-window evidence for an eligible created file | fallback | the file existed in the previous lineage manifest and had earlier AI evidence | current-window authorship or continuity for modified files |
| `deletion` | Inferred from `bash rm` or a provider deletion event | fallback | a captured agent action removed content | line authorship of anything added |
| `none` | No captured AI evidence | - | no AI evidence was captured | that the file is human-authored |

JSON output exposes these values through `evidence_class` and
`evidence_classes`. Human-readable output uses evidence levels and factual
notes.

### Touch origin

`touch_origin` records how a file entered the AI set: `line_level`,
`provider_edit`, `tool_delta`, `deletion`, or `coarse`. Classification prefers
line-level evidence and uses the touch origin when no line matches.

## Per-file evidence: primary vs all

Each file reports:

- `evidence_class` - the strongest class, for display.
  Resolution order: `exact` > `normalized` > `modified`; then, when no line-level
  evidence exists, `tool_delta_touch` > `carry_forward` > `deletion` >
  `provider_touch` > `provider_coarse`.
- `evidence_classes` - all contributing classes, strongest first.

`evidence_classes` preserves weaker corroborating signals even when line-level
evidence determines `evidence_class`.

## Commit evidence level

Each commit has a `High`, `Medium`, or `Low` evidence level and a
`fallback_count`. The level combines:

- line quality, ordered as `exact`, `normalized`, then `modified`;
- a penalty for fallback evidence.

`fallback_count` counts AI-attributed files carrying any fallback evidence,
including files that also have line-level evidence. Each file contributes to at
most one fallback bucket.

Line-level evidence contributes according to match quality. Fallback evidence
subtracts from the score. Coefficients and thresholds are tunable implementation
details.

## Tool-delta evidence (v2)

Tool-window capture records the workspace delta observed while an agent-issued
Bash tool ran. This can include changes from scripts, formatters, and generators.

- `ai_delta_exact_lines` / `ai_delta_formatted_lines` - line matches also backed
  by a verified delta.
- `tool_delta_touch` - a delta touched the file but no line survived to match.

A delta is time-bounded, not exclusive. Concurrent processes can produce changes
within the same window. Only verified single-actor deltas are scored. Partial,
ambiguous, binary, truncated, symlink, gitlink, and unaligned changes retain
file-level evidence only.

## v1 vs v2, and result stamping

- **v1 (default)** uses assistant `Edit`/`Write` output and supports Bash
  deletion inference.
- **v2 (opt-in)** additionally scores verified Bash workspace deltas, producing
  `tool_delta_touch` and the delta-line subsets. Enable with `attribution_v2` in
  `.semantica/settings.json` or `SEMANTICA_ATTRIBUTION_V2=1`.
- Capture and scoring are separate. Semantica may capture eligible Bash deltas
  while v2 scoring is disabled. Enabling v2 does not update existing results.
- Commit-message trailers use v1. `semantica blame`, background enrichment, and
  hosted attribution use the repository's selected version. Each result records
  its `attribution_version`.

## Guarantees and limits

Semantica guarantees that:

- The headline percentage is line-level evidence only; provider-only lines are
  separate.
- The strongest evidence per file is surfaced as `evidence_class`, and every
  contributing class is preserved in `evidence_classes`.
- The commit evidence level discounts fallback signals rather than treating them
  as equal to line-level matches.
- Results record their `attribution_version` and evidence classes.

The evidence does not:

- Establish exclusive authorship. A line match does not rule out a human
  producing the same content.
- Treat provider-touch as equivalent to a line-level match.
- Merge historical and current AI lines within one file. Carry-forward is a
  per-file window heuristic, and a file with current-window activity stays
  current-window authoritative.
