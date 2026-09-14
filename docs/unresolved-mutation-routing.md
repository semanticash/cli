# Unresolved mutation routing

Mutation events need structured destination paths. The broker routes each path to its deepest active registered repository. Session directories associate conversation context with a repository; they do not establish mutation ownership. Codex relative Write/Edit paths are normalized against the provider's working directory before routing.

An event with missing or unmatched mutation paths is retained under `$SEMANTICA_HOME/unresolved-mutations/`. Each private JSON record contains the original event, session and turn identifiers, and the referenced payload and provenance objects. Retention must succeed before advancing a transcript offset. Repeated delivery preserves the first record; an event ID with different content is rejected.

These are potential mutation events, not confirmed file changes. Read-only Bash commands also lack mutation paths. Unknown tools are treated conservatively. `semantica doctor` reports the global retained count, which is not a misattribution rate.

Existing single-repository Bash snapshots continue to capture observed changes. Raw shell events stored alongside those observations carry `mutation_routing: context_only`. They remain available as development context, but cannot supply raw line, deletion, or file-touch attribution. Verified tool deltas are evaluated separately under the existing capture and scoring rules. No additional repositories are scanned.

Provenance bundles preserve the context-only label and any independently linked delta. Consumers must respect that label rather than infer ownership from the bundle's repository or the raw shell command.

## Limits

- Arbitrary shell mutation destinations remain unresolved without trustworthy path evidence. Retrying after execution cannot reconstruct a missing before-state.
- Retained records are local. They are not automatically routed, uploaded, or deleted. A verified local delta does not prove that a command had no effects in other repositories, so its source record remains retained.
- Retention adds an atomic file write and filesystem synchronization for each new unresolved event. It does not add repository snapshots or a new delivery worker.
- Known-destination writes retain their existing persistence behavior. This is not a general delivery acknowledgement protocol.
- Historical rows and completed attribution results are unchanged. Capture-readiness continues to report incomplete tool-window evidence separately.
