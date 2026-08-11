-- Evidence links attach content-addressed evidence blobs to event rows.
-- Event rows are append-only; delta references never overwrite event columns.
create table if not exists agent_event_evidence_links (
    event_id      text    not null references agent_events (event_id)
                          on delete cascade,
    evidence_kind text    not null, -- e.g. tool_delta
    evidence_hash text    not null, -- CAS pointer to the evidence blob
    group_id      text    not null, -- concurrency group that produced the evidence
    created_at    integer not null,
    primary key (event_id, evidence_kind, group_id)
);

-- Supports reference-aware CAS cleanup.
create index if not exists idx_event_evidence_links_hash
    on agent_event_evidence_links (evidence_hash);

-- Supports packaging and diagnostics lookups by group.
create index if not exists idx_event_evidence_links_group
    on agent_event_evidence_links (group_id, event_id);
