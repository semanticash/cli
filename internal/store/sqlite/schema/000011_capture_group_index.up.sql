-- Repair databases that applied migration 6 before the group index was included.
create index if not exists idx_event_evidence_links_group
    on agent_event_evidence_links (group_id, event_id);
