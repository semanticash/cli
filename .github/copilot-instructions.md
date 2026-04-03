Review pull requests for correctness, regressions, and missing tests before style concerns.

Semantica is a Go CLI that captures AI coding activity, stores repo-local state in `.semantica/lineage.db`, integrates with git hooks, and computes commit attribution from hook events and checkpoint data. Prioritize findings in these areas:

- git hook behavior and commit lifecycle correctness
- attribution accuracy and checkpoint window boundaries
- SQLite locking, migrations, and repo-local state consistency
- file path normalization, repo-relative paths, and untracked/non-ignored file handling
- subprocess, shell, auth, and remote push safety

When reviewing:

- Prefer bug-risk comments over style or naming suggestions.
- Call out missing or weak tests for hook flows, attribution logic, SQLite behavior, workflows, and release packaging.
- If SQL queries, schema, or generated DB code change, check that generated files stay in sync.
- If a change affects CLI behavior, installation, release packaging, or GitHub reporting, check whether docs or changelog updates are needed.
- Avoid low-value commentary about formatting, minor refactors, or personal preference unless it hides a real bug.

Repository-specific notes:

- Generated SQLite query code lives under `internal/store/sqlite/db/`.
- Release behavior is driven by `.goreleaser.yaml` and `.github/workflows/`.
- The most sensitive areas are `internal/service/`, `internal/hooks/`, `internal/store/sqlite/`, `internal/git/`, and workflow/config changes under `.github/`.
