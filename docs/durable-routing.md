# Durable capture handoff

Set `SEMANTICA_DURABLE_ROUTING=1` in the agent's environment to enable retained
capture. It is off by default and independent of `SEMANTICA_OBSERVE_ROUTING`.
It does not add repository snapshots or change how the broker selects destinations.
Tool-window publication keeps its existing path.

With this flag enabled, broker-routed events and their payload/provenance objects
are retained before transcript offsets advance. Repository delivery may then fail
without losing the retained capture. The existing worker drain retries pending
records even if the flag is subsequently disabled.

The routing directory lives under `$SEMANTICA_HOME` (default `~/.semantica`):

- `routing/pending/`: events, frozen destination paths, and delivery acknowledgements.
- `routing/objects/`: evidence links owned by each pending event, independent of
  cleanup in the original blob store.
- `routing/delivered/`: compact content fingerprints that reject conflicting replays.

A destination is acknowledged only after required blobs are verified and synced
and its database transaction commits with full synchronization. A failed
second destination does not invalidate the first destination's acknowledgement.
An event without destinations stays pending. There is no destination discovery,
expiry, or automatic deletion policy for unresolved events.

Successful delivery removes the pending event and its retained evidence. Compact
identity receipts remain. Interrupted cleanup can leave unused evidence files;
these are not removed by an age-based sweep.

The strict writer preserves distinct event IDs, including separate hook and
transcript records for the same tool invocation. It does not apply the legacy
writer's heuristic cross-ID deduplication. Same-ID retries verify existing rows
and can restore missing evidence references; conflicting content is rejected.

Session and turn identities travel with retained events. This handoff does not
rebuild previously packaged turn bundles or revise completed attribution results
when evidence arrives later. Test these downstream semantics before enabling the
flag outside development. A repository worker leaves its checkpoint pending if
required retained delivery to that repository fails.

Run the focused failure tests with:

```sh
go test ./internal/broker ./internal/hooks ./internal/service \
  -run 'TestRetained|TestPersistRouted|TestDurableCapture|TestDrainOnce_DeliversRetained'
```

They cover recovery after retention, source-store cleanup, unavailable repository
DBs, failed blob propagation, partial fan-out, interrupted acknowledgement,
conflicting replay, scoped capture, and offset preservation on retention failure.
