# Capture readiness and attribution

Before attributing a commit, the worker records the registered tool windows that
can overlap its checkpoint. This record is stored in `checkpoint_capture` in the
repository's `lineage.db`. It includes the frozen window identities, the checkpoint
boundary, a retry deadline, and any capture gaps.

The worker tries existing recovery before scoring. Recovery uses recorded deltas
or frozen post trees. It never takes a new workspace snapshot to replace a missing
command completion.

Unsettled capture postpones attribution for up to 30 seconds from the first
inspection, with retries every 5 seconds. These waits release the checkpoint lease
and do not consume the processing-failure attempt budget. The existing repository
queue remains ordered. Other repositories can continue independently.

After the deadline, the worker records incomplete capture and completes the
checkpoint with partial attribution. It does not forcibly close a command that
might still be running. Normal tool-window recovery remains responsible for
reclaiming stale registrations and refs.

## Results

When capture is incomplete, `blame` displays `Unattributed` instead of `Human`
for unmatched lines and labels the percentage `AI matched`. JSON results and
attribution uploads include `capture` and `unattributed_lines`. Their human line
counts are zero, and per-file results carry the same distinction. Consumers must
honor these fields rather than infer human authorship from `total_lines - ai_lines`.

The gap remains attached to the checkpoint if evidence arrives later. Completed
checkpoints are not automatically re-attributed. `blame` still recomputes matches
from available evidence, but retains the saved capture qualification. Audit
readiness does not report incomplete capture as ready.

This is a bounded check of known tool-window capture, not proof that every agent
action was observed. Legacy checkpoints without a capture record receive a
read-only assessment of remaining registrations and available evidence. Missing
historical state cannot be reconstructed from an empty registry.

Concurrent-group deltas remain subject to the existing attribution rules. Settling
a group does not establish exclusive authorship for any one member.

## Diagnosing missing completion

Set `SEMANTICA_CAPTURE_TRACE=1` in the environment inherited by capture hooks.
Semantica emits lifecycle receipt, registration, and completion diagnostics to
stderr with session, turn, and tool-use identities. It does not log command text,
prompts, or responses. Preserve the hook runner's stderr when tracing.

A registration without completion does not identify which layer lost the post
hook. Compare provider delivery logs with Semantica's receipt and registry logs.
Process exit alone is not a substitute for a trustworthy captured post-state.
