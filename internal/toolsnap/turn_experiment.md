# Turn snapshot experiment

The `turnexperiment` build tag enables a test-only harness. Production hooks,
workers, routing, attribution, health, and uploads are unchanged.

Run the controlled cases:

```sh
go test -tags turnexperiment ./internal/toolsnap -run '^TestTurnExperiment' -count=1 -v
```

The harness freezes repository IDs and worktree identities before taking any
baseline. Repository observations run with concurrency 1 or full parallelism.
Both boundaries wait for every observation. Each repository owns its result
slot and temporary store. The harness reuses the existing snapshot and tree
delta primitives; production `CaptureAfter` is unchanged.

Results are `changed`, `unchanged`, or `unknown`. A changed result means either
a supported net worktree difference or a changed intermediate commit tree,
not agent authorship. Commit boundaries compare each commit with its parent;
the final worktree compares with the original worktree baseline. Keeping these
comparisons separate avoids treating pre-existing dirt as deleted at a commit.
Files are reported once in the result, with per-boundary file lists retained.

Restoring contents without a commit remains unchanged. A commit followed by a
reverting commit remains changed because both committed boundaries are retained.
Empty commits alone remain unchanged. Only a linear continuation of the starting
HEAD is supported; rewrites and merges produce unknown. The harness reads
reachable commit history at turn end, not reflogs. Commits made and reset away
before turn end cannot be recovered when the ending HEAD hides that history.
Snapshot errors, changed identities, truncated deltas, unsupported index states,
and uncertain completion still produce unknown.

Supported evidence includes regular-file contents, binary contents, symlink
targets, and Git file modes. Ignored files and Semantica's internal paths are
excluded. Filesystem metadata outside Git's representation is not compared.
Assume-unchanged, skip-worktree, unmerged entries, and tracked submodules are
rejected. A snapshot is not an atomic filesystem transaction, and the seven
repositories do not share a single instantaneous boundary.

For the controlled fixtures, the caller supplies completion certainty. The background-command test owns a
live child and explicitly supplies uncertainty. This tests the unknown result,
not detection of background processes or reliability of provider notifications.
Direct file writes model actor-independent state changes; those fixtures invoke
no provider adapter. Overlapping turns can both observe a change without identifying its
author.

## Local timing

Supply a JSON array using lineage repository IDs, not broker registration IDs:

```json
[
  {"repository_id": "repository-a", "path": "/absolute/path/to/a"},
  {"repository_id": "repository-b", "path": "/absolute/path/to/b"}
]
```

```sh
SEMANTICA_TURN_EXPERIMENT_REPOS=/tmp/repos.json \
SEMANTICA_TURN_EXPERIMENT_OUTPUT=/tmp/turn-results.json \
go test -tags turnexperiment ./internal/toolsnap \
  -run '^TestTurnExperimentRealRepos$' -count=1 -v
```

This launches no workload and changes no repository files. It reads current
worktrees and writes only temporary snapshot objects and the requested JSON
report. Run it without concurrent builds or other benchmarks. External writers
are not tracked, so this is an idle-cost measurement, not a certified turn-end
capture.

Each concurrency mode gets one new-store run and 30 reused-store runs. Mode
order alternates each iteration, using separate stores and the same subject
list. New-store does not mean cold OS caches. Each turn has a two-minute
shared context across start and end; timeout results remain unknown and are not
discarded. Timings include worktree resolution, identity and index checks,
store opening, snapshots, and end-state comparison. Snapshot calls are also
timed separately. Repository-list loading, report serialization, and storage
measurement are outside the timed boundaries.

Stored bytes are logical file sizes in the temporary stores, including their
administrative files. Existing repository objects are read through alternates
and are not copied. This is not process memory usage or a projection of storage
growth across changing turns.

Report median, nearest-rank p95, maximum, and the new-store run separately. Count
unknown results alongside timings. Choose a latency threshold before using the
measurements as an integration gate.

## Changed workload

`TestTurnExperimentChangedWorkload` is separately opt-in. It uses the same seven
live repositories but writes fixtures only in `sample` and `sample2`. Both must
start clean. It uses temporary detached HEADs, disables hooks/signing only for
its own Git commands, and restores the original checkouts afterward. The
original branches are not advanced. Git may retain the temporary commit objects
and reflog entries until normal garbage collection.

```sh
SEMANTICA_TURN_EXPERIMENT_CHANGED=1 \
SEMANTICA_TURN_EXPERIMENT_REPOS=/tmp/repos.json \
SEMANTICA_TURN_EXPERIMENT_OUTPUT=/tmp/changed-results.json \
go test -tags turnexperiment ./internal/toolsnap \
  -run '^TestTurnExperimentChangedWorkload$' -count=1 -v
```

Each target gets 24 tracked fixture files of 256 lines. Each turn edits 16 files
in each target, replacing every fourth line. Sample also gets an untracked file.
Sample2 commits its edits, then modifies a seventeenth tracked file. Baseline
restoration and baseline commits happen outside the snapshot timings. Workload
execution is timed separately from the start/end boundaries.

The test runs one warmup and 30 warm-store turns without contention, then the
same workload with two paced SHA-256 workers and one paced filesystem writer.
The writer cycles eight 1 MiB files in a temporary directory, syncing each write
and waiting 50 ms. Each CPU worker hashes 32 MiB per batch and waits 10 ms.
Pressure runs continuously through the contended block, including baseline
preparation. No source repository is used for the pressure files. Unknown
observations stay in the timing report; no samples are dropped.

## Real lifecycle boundaries

`TestTurnExperimentLifecycle` launches one real CLI invocation per case. A
temporary synchronous hook invokes `TestTurnExperimentHook` in the compiled
test binary. That helper uses the Codex or Claude adapter to parse start/end
payloads and forwards them to an in-memory HTTP receiver. It never calls
`Dispatch`. User/provider hook files are not installed or modified.

The receiver runs the existing parallel baseline on `UserPromptSubmit` and
reconciliation on `Stop`, waiting before returning each hook request. Tool hooks
record timing only. They take no snapshots and supply no repository routing.
The configured seven active subjects are copied into the receiver; worktree
identities and baselines are frozen when the start hook arrives. Run with a
stable registration set matching the supplied JSON list.

```sh
go test -tags turnexperiment -c -o /tmp/turn-lifecycle.test ./internal/toolsnap
SEMANTICA_TURN_EXPERIMENT_PROVIDER=codex \
SEMANTICA_TURN_EXPERIMENT_REPOS=/tmp/repos.json \
SEMANTICA_TURN_EXPERIMENT_OUTPUT=/tmp/codex-turns.json \
/tmp/turn-lifecycle.test -test.run '^TestTurnExperimentLifecycle$' -test.v
```

Use `claude` for the other provider. Both use existing subscription authentication
and their default model selection. The two sample repositories must start clean.
Only owned fixtures are changed, on detached HEADs restored after the run.
Codex's hook-trust bypass applies to the vetted temporary hook definitions for
that invocation; its tool sandbox and automatic approval review remain enabled.
Claude loads the temporary settings with user/project/local settings excluded.

The six default cases cover local editing, opaque cross-repo Bash, cross-repo
file editing, a commit, a commit followed by dirty changes, and no changes.
Set `SEMANTICA_TURN_EXPERIMENT_CASE=background` for the separate owned-child
control. That control uses marker files and a process-state probe for its own
child; it does not implement general background-process detection.

Missing, duplicate, out-of-order, mismatched, or uncertain boundaries make the
entire turn unknown. No replacement boundary is inferred. One invocation owns
one turn, so a provider session ID can pair Claude's start/end signals without
a provider turn ID. This does not test concurrent turns within one session,
resumes, desktop integrations, or provider termination during a turn.

The foreground fixtures contain known synchronous workloads. A successful Stop
does not establish quiescence for arbitrary programs. Background requests,
returned execution sessions, or an unfinished owned child prevent that claim.
There is no complete detector for a script that hides background work.

The report retains hook arrivals, normalized identities, boundary completion,
provider exit, results, and separate provider logs. Snapshot timings exclude
agent inference/startup and hook subprocess/HTTP overhead. Inspect the report's
issues and expected/observed destinations; a measurement test completing does
not itself mean the provider passed its acceptance criteria.

## Completion signals

`TestTurnExperimentCompletionSignals` records provider capabilities without
calling any snapshot or lifecycle-reconciliation code. It reuses only the hook
relay, preserves complete raw hook payloads, and records the CLI event stream.
All workloads and hook settings live in temporary directories.

```sh
go test -tags turnexperiment -c -o /tmp/completion.test ./internal/toolsnap
SEMANTICA_COMPLETION_PROVIDER=codex \
SEMANTICA_COMPLETION_OUTPUT=/tmp/completion-codex \
/tmp/completion.test -test.run '^TestTurnExperimentCompletionSignals$' -test.v
```

Use `claude` for the other provider. The cases are `foreground`, `managed`,
`detached`, and `managed_join`; `SEMANTICA_COMPLETION_CASE` selects one case.
The managed cases use the provider's own background execution mechanism.
The detached case starts a separate-session supervisor and returns without
exposing a task handle to the provider.

The fixture reports its child's start, blocks on an explicit release barrier,
and reports the return code after its own `subprocess.run` waits for the child.
For non-joined background cases, the receiver records Stop before releasing the
fixture. The joined control uses a separate release command followed by the
provider's native task-result/wait tool. These are experimental stimuli, not
completion heuristics. There are no process scans, PID files, or grace periods.

Provider signals and fixture ground truth are recorded separately. A fixture
exit report must not become provider evidence. `fixture_state` is the last
reported state, not a live process assertion: a missing exit report leaves
completion unknown. The three-minute overall test deadline limits collection;
reaching it never establishes completion. Failure events are recorded when the
provider exposes them; no failing-command scenario is included in this matrix.

Output is one JSON trace and one raw CLI log per case. Measurements may finish
with missing evidence, so test-run success alone is not a settled verdict.
No production provider configuration, adapter, or capture behavior is changed.
