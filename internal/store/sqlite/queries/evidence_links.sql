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

-- name: ListEvidenceLinksInWindow :many
-- Lists tool-delta links and session providers in event-window order,
-- with event recency for deterministic winner selection.
select
    l.event_id,
    l.evidence_hash,
    l.group_id,
    s.provider,
    e.ts,
    e.insert_seq
from agent_event_evidence_links l
    join agent_events e on e.event_id = l.event_id
    join agent_sessions s
        on s.session_id = e.session_id
        and s.repository_id = e.repository_id
where e.repository_id = ?
    and l.evidence_kind = 'tool_delta'
    and ((cast(sqlc.arg(use_cursor) as integer) = 1
            and (e.ts > sqlc.arg(after_ts)
                 or (e.ts = sqlc.arg(after_ts) and e.insert_seq > sqlc.arg(after_cursor)))
            and (e.ts < sqlc.arg(up_to_ts)
                 or (e.ts = sqlc.arg(up_to_ts) and e.insert_seq <= sqlc.arg(up_to_cursor))))
         or (cast(sqlc.arg(use_cursor) as integer) = 0
            and e.ts > sqlc.arg(after_ts) and e.ts <= sqlc.arg(up_to_ts)))
order by e.ts, e.insert_seq, l.event_id, l.group_id;
