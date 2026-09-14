# Capture readiness and attribution

Before attributing a commit, the worker saves the relevant registered tool windows,
checkpoint boundary, retry deadline, and capture gaps in `checkpoint_capture` in
the repository's `lineage.db`.

Recovery uses stored deltas or frozen post trees. It never takes a new workspace
snapshot to replace a missing command completion.

Unsettled capture receives a 30-second grace period from the first inspection,
with retries every 5 seconds. Waits release repository locks and checkpoint leases
without consuming processing attempts. Checkpoints remain ordered within each
repository; other repositories can continue independently.

The standalone `worker run` process waits for capture retries without a launcher
or another commit. Cancellation stops the wait. Processing failures retain their
existing retry behavior.

If capture is still unsettled at the deadline, the worker records incomplete
capture and proceeds with partial attribution. It does not forcibly close active
commands. Tool-window recovery reclaims stale registrations and refs.

## Results

When capture is incomplete, `blame` and `explain` label unmatched lines
`Unattributed` and the percentage `AI matched`. JSON results and attribution uploads
include `capture` and `unattributed_lines`, with zero human lines. Per-file results
use the same distinction. `total_lines - ai_lines` does not establish human
authorship.

Explain's model context and skill output preserve this uncertainty. Mixed files
count under both `files_with_ai` and `files_unattributed`. `status` omits incomplete
checkpoints from its AI trend.

Recorded gaps remain if evidence arrives later. Completed checkpoints are not
automatically re-attributed. `blame` recomputes matches but retains the saved
capture status. Audit readiness does not report incomplete capture as ready.

Capture readiness covers registered tool windows, not every possible agent action.
Legacy checkpoints without a capture record are assessed from surviving state.
An empty registry cannot establish historical capture completeness.

The checkpoint window selects relevant groups; validation checks their full
persisted membership. Older members outside the window still require valid links.
Settling a concurrent group does not establish exclusive authorship for a member.

Registry windows have timestamps but no event cursors. Lower-boundary timestamp
ties remain potentially relevant. A post-state captured in the same millisecond
as the checkpoint produces `post_state_order_unknown`. Timestamp ties can therefore
cause conservative capture gaps even when evidence was collected.

## Diagnosing missing completion

Set `SEMANTICA_CAPTURE_TRACE=1` in the environment inherited by capture hooks.
Semantica emits lifecycle receipt, registration, and completion diagnostics to
stderr with session, turn, and tool-use identities. It does not log command text,
prompts, or responses. Preserve the hook runner's stderr when tracing.

A registration without completion does not identify which layer lost the post
hook. Compare provider delivery logs with Semantica's receipt and registry logs.
Process exit alone is not a substitute for a trustworthy captured post-state.
