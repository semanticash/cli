-- name: InsertEvidenceLinkIfAbsent :exec
-- Idempotent creation half; the caller re-reads the row and fails closed
-- on an evidence hash mismatch instead of overwriting it.
insert into agent_event_evidence_links (event_id, evidence_kind, evidence_hash, group_id, created_at)
values (?, ?, ?, ?, ?)
on conflict (event_id, evidence_kind, group_id) do nothing;

-- name: GetEvidenceLink :one
select * from agent_event_evidence_links
where event_id = ? and evidence_kind = ? and group_id = ?;

-- name: ListEvidenceLinksByGroup :many
select * from agent_event_evidence_links
where group_id = ? order by event_id;

-- name: ListEvidenceLinksByEvent :many
select * from agent_event_evidence_links
where event_id = ? order by evidence_kind, group_id;

-- name: AgentEventExists :one
select count(*) > 0 as exists_flag from agent_events
where event_id = ?;
