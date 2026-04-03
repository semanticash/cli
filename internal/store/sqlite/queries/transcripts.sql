-- name: ListTranscriptEvents :many
select
    e.event_id, e.session_id, s.provider,
    e.ts, e.kind, e.role, e.tool_uses,
    e.tokens_in, e.tokens_out, e.tokens_cache_read, e.tokens_cache_create,
    e.summary, e.provider_event_id, e.payload_hash
from agent_events e
    join agent_sessions s on s.session_id = e.session_id
where e.repository_id = ?
    and e.ts > sqlc.arg(after_ts)
    and e.ts <= sqlc.arg(until_ts)
order by e.ts, s.provider, e.session_id, e.event_id;

-- name: ListEventsInWindow :many
-- Returns all events for a repository within a time window, without
-- requiring session_checkpoints links. Used by attribution to query
-- events directly by repository and time range.
select
    e.event_id,
    e.session_id,
    s.provider,
    s.model,
    e.ts,
    e.kind,
    e.role,
    e.tool_uses,
    e.tokens_in,
    e.tokens_out,
    e.tokens_cache_read,
    e.tokens_cache_create,
    e.summary,
    e.provider_event_id,
    e.payload_hash
from agent_events e
    join agent_sessions s on s.session_id = e.session_id
where e.repository_id = ?
    and e.ts > sqlc.arg(after_ts)
    and e.ts <= sqlc.arg(up_to_ts)
order by e.ts, e.event_id;
