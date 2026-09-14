-- name: ListCaptureGroupLinks :many
-- Validate full group membership independently of the attribution window.
select l.event_id, l.evidence_hash, s.provider, e.session_id, e.turn_id
from agent_event_evidence_links l
join agent_events e on e.event_id = l.event_id
join agent_sessions s on s.session_id = e.session_id and s.repository_id = e.repository_id
where l.group_id = ? and l.evidence_kind = 'tool_delta' and e.repository_id = ?
order by l.event_id;
