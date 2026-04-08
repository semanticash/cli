-- name: UpsertAgentSource :one
insert into agent_sources (
    source_id, repository_id, provider, source_key, last_seen_at, created_at
) values (?, ?, ?, ?, ?, ?)
on conflict(repository_id, provider, source_key) do update set
    last_seen_at=excluded.last_seen_at
returning *;

-- name: UpsertAgentSession :one
insert into agent_sessions (
    session_id, provider_session_id, parent_session_id,
    repository_id, provider, source_id,
    started_at, last_seen_at, metadata_json,
    source_repo_path, model
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(repository_id, provider, provider_session_id) do update set
    parent_session_id=excluded.parent_session_id,
    last_seen_at=excluded.last_seen_at,
    metadata_json=excluded.metadata_json,
    source_repo_path=coalesce(agent_sessions.source_repo_path, excluded.source_repo_path),
    model=coalesce(excluded.model, agent_sessions.model)
returning *;

-- name: GetAgentSessionByProviderID :one
select * from agent_sessions
where repository_id = ? and provider = ? and provider_session_id = ?;

-- name: ListAgentSessionsByProviderSessionID :many
-- Search by provider_session_id across all providers in a repo.
-- Returns multiple rows if different providers reuse the same ID.
select * from agent_sessions
where repository_id = ? and provider_session_id = ?;

-- name: InsertAgentEvent :exec
insert or ignore into agent_events (
    event_id, session_id, repository_id, ts, kind,
    payload_hash, role, tool_uses,
    tokens_in, tokens_out, tokens_cache_read, tokens_cache_create,
    summary, provider_event_id,
    turn_id, tool_use_id, tool_name, event_source,
    provenance_hash
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: StepEventExists :one
select count(*) > 0 as exists_flag from agent_events
where turn_id = ? and tool_use_id = ? and tool_name = ?;

-- name: PromptEventExists :one
select count(*) > 0 as exists_flag from agent_events
where turn_id = ?
  and role = 'user'
  and kind = 'user'
  and event_source = 'hook';

-- name: ListAgentEventsBySession :many
select * from agent_events where session_id = ? order by ts desc limit ?;

-- name: ListAgentEventsBySessionPaged :many
-- Keyset pagination: returns the next page of events after the given cursor.
-- Use after_ts=0, after_event_id='' for the first page. Order is ascending
-- (chronological) for timeline construction.
select * from agent_events
where session_id = sqlc.arg(session_id)
  and (ts > sqlc.arg(after_ts)
       or (ts = sqlc.arg(after_ts) and event_id > sqlc.arg(after_event_id)))
order by ts asc, event_id asc
limit sqlc.arg(page_limit);

-- name: GetActiveAgentSessionForRepo :one
select * from agent_sessions where repository_id = ? order by last_seen_at desc limit 1;

-- name: InsertSessionCheckpoint :exec
insert or ignore into session_checkpoints (session_id, checkpoint_id) values (?, ?);

-- name: ListSessionsForCheckpoint :many
select s.* from agent_sessions s
    join session_checkpoints sc on sc.session_id = s.session_id
where sc.checkpoint_id = ? order by s.started_at;

-- name: ListSessionsForRepository :many
select * from agent_sessions where repository_id = ?
order by last_seen_at desc limit ?;

-- name: ListSessionsWithStats :many
-- tokens_in is the non-cached input token count reported by the provider.
-- tokens_cached is cache_read + cache_create. Claude's API reports input_tokens
-- as only the non-cached portion; with heavy prompt caching the cached portion
-- can be >99% of actual context, so we report them separately for clarity.
with session_scope as (
    select * from agent_sessions as base where base.repository_id = ?
),
event_stats as (
    select
        s.session_id,
        cast(coalesce(max(e.ts), s.last_seen_at) as integer) as last_event_ts,
        cast(count(e.event_id) as integer) as step_count,
        cast(coalesce(sum(case when e.role = 'assistant' and (
            (e.tool_name is not null and e.tool_name != '')
            or (
                e.tool_uses is not null and e.tool_uses != '' and (
                    (json_type(e.tool_uses) = 'array' and json_array_length(e.tool_uses) > 0)
                    or coalesce(json_array_length(json_extract(e.tool_uses, '$.tools')), 0) > 0
                )
            )
        ) then 1 else 0 end), 0) as integer) as tool_call_count
    from session_scope s
    left join agent_events e on e.session_id = s.session_id
    group by s.session_id
),
token_groups as (
    select
        e.session_id,
        coalesce(e.tokens_in, 0) as tokens_in,
        coalesce(e.tokens_out, 0) as tokens_out,
        coalesce(e.tokens_cache_read, 0) as tokens_cache_read,
        coalesce(e.tokens_cache_create, 0) as tokens_cache_create,
        row_number() over (
            partition by
                e.session_id,
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then e.provider_event_id
                    else e.event_id
                end
            order by
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then coalesce(e.tokens_out, 0)
                    else 0
                end desc,
                e.ts desc,
                e.event_id desc
        ) as rn
    from agent_events e
    join session_scope s on s.session_id = e.session_id
),
token_stats as (
    select
        session_id,
        cast(coalesce(sum(tokens_in), 0) as integer) as tokens_in,
        cast(coalesce(sum(tokens_out), 0) as integer) as tokens_out,
        cast(coalesce(sum(tokens_cache_read), 0) + coalesce(sum(tokens_cache_create), 0) as integer) as tokens_cached
    from token_groups
    where rn = 1
    group by session_id
)
select
    s.session_id, s.provider_session_id, s.parent_session_id,
    s.provider, s.started_at,
    es.last_event_ts,
    es.step_count,
    es.tool_call_count,
    cast(coalesce(ts.tokens_in, 0) as integer) as tokens_in,
    cast(coalesce(ts.tokens_out, 0) as integer) as tokens_out,
    cast(coalesce(ts.tokens_cached, 0) as integer) as tokens_cached
from session_scope s
join event_stats es on es.session_id = s.session_id
left join token_stats ts on ts.session_id = s.session_id
where es.step_count > 0
order by es.last_event_ts desc
limit ?;

-- name: ListSessionsWithStatsAll :many
-- Same as ListSessionsWithStats but includes sessions with zero events.
with session_scope as (
    select * from agent_sessions as base where base.repository_id = ?
),
event_stats as (
    select
        s.session_id,
        cast(coalesce(max(e.ts), s.last_seen_at) as integer) as last_event_ts,
        cast(count(e.event_id) as integer) as step_count,
        cast(coalesce(sum(case when e.role = 'assistant' and (
            (e.tool_name is not null and e.tool_name != '')
            or (
                e.tool_uses is not null and e.tool_uses != '' and (
                    (json_type(e.tool_uses) = 'array' and json_array_length(e.tool_uses) > 0)
                    or coalesce(json_array_length(json_extract(e.tool_uses, '$.tools')), 0) > 0
                )
            )
        ) then 1 else 0 end), 0) as integer) as tool_call_count
    from session_scope s
    left join agent_events e on e.session_id = s.session_id
    group by s.session_id
),
token_groups as (
    select
        e.session_id,
        coalesce(e.tokens_in, 0) as tokens_in,
        coalesce(e.tokens_out, 0) as tokens_out,
        coalesce(e.tokens_cache_read, 0) as tokens_cache_read,
        coalesce(e.tokens_cache_create, 0) as tokens_cache_create,
        row_number() over (
            partition by
                e.session_id,
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then e.provider_event_id
                    else e.event_id
                end
            order by
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then coalesce(e.tokens_out, 0)
                    else 0
                end desc,
                e.ts desc,
                e.event_id desc
        ) as rn
    from agent_events e
    join session_scope s on s.session_id = e.session_id
),
token_stats as (
    select
        session_id,
        cast(coalesce(sum(tokens_in), 0) as integer) as tokens_in,
        cast(coalesce(sum(tokens_out), 0) as integer) as tokens_out,
        cast(coalesce(sum(tokens_cache_read), 0) + coalesce(sum(tokens_cache_create), 0) as integer) as tokens_cached
    from token_groups
    where rn = 1
    group by session_id
)
select
    s.session_id, s.provider_session_id, s.parent_session_id,
    s.provider, s.started_at,
    es.last_event_ts,
    es.step_count,
    es.tool_call_count,
    cast(coalesce(ts.tokens_in, 0) as integer) as tokens_in,
    cast(coalesce(ts.tokens_out, 0) as integer) as tokens_out,
    cast(coalesce(ts.tokens_cached, 0) as integer) as tokens_cached
from session_scope s
join event_stats es on es.session_id = s.session_id
left join token_stats ts on ts.session_id = s.session_id
order by es.last_event_ts desc
limit ?;

-- name: GetSessionWithStats :one
-- Single session with computed stats. Same columns as ListSessionsWithStats.
with session_scope as (
    select * from agent_sessions as base where base.session_id = ?
),
event_stats as (
    select
        s.session_id,
        cast(coalesce(max(e.ts), s.last_seen_at) as integer) as last_event_ts,
        cast(count(e.event_id) as integer) as step_count,
        cast(coalesce(sum(case when e.role = 'assistant' and (
            (e.tool_name is not null and e.tool_name != '')
            or (
                e.tool_uses is not null and e.tool_uses != '' and (
                    (json_type(e.tool_uses) = 'array' and json_array_length(e.tool_uses) > 0)
                    or coalesce(json_array_length(json_extract(e.tool_uses, '$.tools')), 0) > 0
                )
            )
        ) then 1 else 0 end), 0) as integer) as tool_call_count
    from session_scope s
    left join agent_events e on e.session_id = s.session_id
    group by s.session_id
),
token_groups as (
    select
        e.session_id,
        coalesce(e.tokens_in, 0) as tokens_in,
        coalesce(e.tokens_out, 0) as tokens_out,
        coalesce(e.tokens_cache_read, 0) as tokens_cache_read,
        coalesce(e.tokens_cache_create, 0) as tokens_cache_create,
        row_number() over (
            partition by
                e.session_id,
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then e.provider_event_id
                    else e.event_id
                end
            order by
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then coalesce(e.tokens_out, 0)
                    else 0
                end desc,
                e.ts desc,
                e.event_id desc
        ) as rn
    from agent_events e
    join session_scope s on s.session_id = e.session_id
),
token_stats as (
    select
        session_id,
        cast(coalesce(sum(tokens_in), 0) as integer) as tokens_in,
        cast(coalesce(sum(tokens_out), 0) as integer) as tokens_out,
        cast(coalesce(sum(tokens_cache_read), 0) + coalesce(sum(tokens_cache_create), 0) as integer) as tokens_cached
    from token_groups
    where rn = 1
    group by session_id
)
select
    s.session_id, s.provider_session_id, s.parent_session_id,
    s.provider, s.started_at,
    es.last_event_ts,
    es.step_count,
    es.tool_call_count,
    cast(coalesce(ts.tokens_in, 0) as integer) as tokens_in,
    cast(coalesce(ts.tokens_out, 0) as integer) as tokens_out,
    cast(coalesce(ts.tokens_cached, 0) as integer) as tokens_cached
from session_scope s
join event_stats es on es.session_id = s.session_id
left join token_stats ts on ts.session_id = s.session_id;

-- name: GetAgentSessionByID :one
select * from agent_sessions where session_id = ?;

-- name: ListEventsBySessionASC :many
select e.*, s.provider from agent_events e
    join agent_sessions s on s.session_id = e.session_id
where e.session_id = ? order by e.ts, e.event_id;

-- name: ListSessionsWithEventsInWindow :many
-- Returns distinct session IDs that have at least one event in the given
-- time window [after_ts, up_to_ts] for the specified repository.
select distinct e.session_id from agent_events e
where e.repository_id = ?
  and e.ts > sqlc.arg(after_ts)
  and e.ts <= sqlc.arg(up_to_ts);

-- name: ResolveSessionByPrefix :many
select session_id from agent_sessions
where session_id like ? and repository_id = ?
limit 2;

-- name: ListRecentSessionsWithStats :many
-- Same token separation as ListSessionsWithStats (see comment above).
with session_scope as (
    select * from agent_sessions as base
    where base.repository_id = ? and base.last_seen_at > sqlc.arg(since_ts)
),
event_stats as (
    select
        s.session_id,
        cast(coalesce(max(e.ts), s.last_seen_at) as integer) as last_event_ts,
        cast(count(e.event_id) as integer) as step_count
    from session_scope s
    left join agent_events e on e.session_id = s.session_id
    group by s.session_id
),
token_groups as (
    select
        e.session_id,
        coalesce(e.tokens_in, 0) as tokens_in,
        coalesce(e.tokens_out, 0) as tokens_out,
        coalesce(e.tokens_cache_read, 0) as tokens_cache_read,
        coalesce(e.tokens_cache_create, 0) as tokens_cache_create,
        row_number() over (
            partition by
                e.session_id,
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then e.provider_event_id
                    else e.event_id
                end
            order by
                case
                    when e.role = 'assistant' and e.provider_event_id is not null and e.provider_event_id != ''
                        then coalesce(e.tokens_out, 0)
                    else 0
                end desc,
                e.ts desc,
                e.event_id desc
        ) as rn
    from agent_events e
    join session_scope s on s.session_id = e.session_id
),
token_stats as (
    select
        session_id,
        cast(coalesce(sum(tokens_in), 0) as integer) as tokens_in,
        cast(coalesce(sum(tokens_out), 0) as integer) as tokens_out,
        cast(coalesce(sum(tokens_cache_read), 0) + coalesce(sum(tokens_cache_create), 0) as integer) as tokens_cached
    from token_groups
    where rn = 1
    group by session_id
)
select
    s.session_id, s.provider_session_id, s.parent_session_id,
    s.provider, s.started_at,
    es.last_event_ts,
    es.step_count,
    cast(coalesce(ts.tokens_in, 0) as integer) as tokens_in,
    cast(coalesce(ts.tokens_out, 0) as integer) as tokens_out,
    cast(coalesce(ts.tokens_cached, 0) as integer) as tokens_cached
from session_scope s
join event_stats es on es.session_id = s.session_id
left join token_stats ts on ts.session_id = s.session_id
where es.step_count > 0
order by es.last_event_ts desc
limit ?;

-- name: ListCrossRepoSessions :many
select * from agent_sessions
where repository_id = ? and source_repo_path is not null
order by last_seen_at desc limit ?;

-- name: ListDistinctProviders :many
select distinct provider from agent_sessions where repository_id = ?;

-- name: ListProvidersByCheckpoint :many
select distinct s.provider from agent_sessions s
    join session_checkpoints sc on sc.session_id = s.session_id
where sc.checkpoint_id = ?;

-- name: UpsertProvenanceManifest :exec
insert into provenance_manifests (
    manifest_id, repository_id, session_id, turn_id, provider, kind,
    transcript_ref, provenance_bundle_hash,
    started_at, completed_at, status,
    upload_attempts, created_at, updated_at
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
on conflict(repository_id, session_id, turn_id, kind) do update set
    transcript_ref=excluded.transcript_ref,
    provenance_bundle_hash=excluded.provenance_bundle_hash,
    completed_at=excluded.completed_at,
    status=excluded.status,
    updated_at=excluded.updated_at;

-- name: MarkManifestUploaded :exec
update provenance_manifests set status = 'uploaded', updated_at = ? where manifest_id = ?;

-- name: MarkManifestFailed :exec
update provenance_manifests set
    status = 'failed',
    upload_attempts = upload_attempts + 1,
    last_error = ?,
    updated_at = ?
where manifest_id = ?;

-- name: ListPendingManifests :many
select * from provenance_manifests
where repository_id = ? and status in ('pending', 'packaged')
order by created_at
limit ?;

-- name: MarkManifestUploading :execrows
-- Guards on status='packaged' so concurrent sync workers cannot both claim
-- the same manifest. Caller checks rows affected: 0 means already claimed.
update provenance_manifests set status = 'uploading', updated_at = ?
where manifest_id = ? and status = 'packaged';

-- name: ListPackagedManifests :many
-- Returns manifests ready for upload, scoped to a watermark timestamp.
-- Only packaged status with attempts below the retry cap. Pass 0 for
-- watermark_ts to drain all.
select * from provenance_manifests
where repository_id = ? and status = 'packaged'
  and upload_attempts < 5
  and (? = 0 or created_at <= ?)
order by created_at
limit ?;

-- name: RecoverStaleUploading :exec
-- Resets manifests stuck in uploading after a crash. Threshold is a unix ms
-- timestamp; rows with updated_at older than this are reset to packaged.
update provenance_manifests set status = 'packaged', updated_at = ?
where status = 'uploading' and updated_at < ?;

-- name: ListStepProvenanceForTurn :many
-- Returns step provenance hashes for a turn, used to build the upload envelope.
select event_id, tool_use_id, tool_name, provenance_hash
from agent_events
where session_id = ? and turn_id = ?
  and provenance_hash is not null
  and tool_name is not null
order by ts;

-- name: GetPromptEventForTurn :one
select event_id, payload_hash from agent_events
where session_id = ? and turn_id = ?
  and role = 'user' and kind = 'user' and event_source = 'hook'
limit 1;

-- name: ListStepEventsForTurn :many
-- Returns step events for provenance bundle packaging.
-- Includes both hook-captured and transcript-sourced events
-- so providers without direct hook capture are covered.
select event_id, ts, tool_name, tool_use_id, provenance_hash, payload_hash,
       summary, event_source, tool_uses
from agent_events
where session_id = ? and turn_id = ?
  and tool_name is not null
order by ts;

-- name: GetManifestCommitLink :one
-- Resolves the commit link for the checkpoint that covers a manifest.
-- First finds the earliest checkpoint in the session created at or after
-- the manifest's start time (the covering checkpoint), then joins to
-- commit_links. Returns no rows if the covering checkpoint has no commit.
with covering_checkpoint as (
    select cp.checkpoint_id
    from session_checkpoints sc
    join checkpoints cp on sc.checkpoint_id = cp.checkpoint_id
    where sc.session_id = ?
      and cp.created_at >= ?
    order by cp.created_at asc
    limit 1
)
select cl.commit_hash, cl.checkpoint_id
from covering_checkpoint cc
join commit_links cl on cc.checkpoint_id = cl.checkpoint_id;

-- name: ListStepCompanionResults :many
-- Returns tool_result events matching a step's tool_use_id, ordered by
-- timestamp. Returned as :many so the provider enricher can choose the
-- right pairing when multiple results exist (e.g., retried tool calls).
select event_id, payload_hash, summary, role, kind, ts
from agent_events
where session_id = ?
  and turn_id = ?
  and tool_use_id = ?
  and kind = 'tool_result'
order by ts asc;

-- name: GetNextToolResultAfter :one
-- Temporal fallback for companion matching when tool_use_id is empty.
-- Returns the first tool_result in the same turn after the given timestamp.
-- Used by Claude transcript enrichment where tool_result rows lack tool_use_id.
select event_id, payload_hash, summary, role, kind, ts
from agent_events
where session_id = ?
  and turn_id = ?
  and kind = 'tool_result'
  and ts > ?
order by ts asc
limit 1;

-- name: ResetManifestForRetry :exec
-- Resets a failed or uploading manifest back to packaged for retry.
-- Increments upload_attempts and records the error for diagnostics.
update provenance_manifests set
    status = 'packaged',
    upload_attempts = upload_attempts + 1,
    last_error = ?,
    updated_at = ?
where manifest_id = ?;

-- name: ResetManifestToPackaged :exec
-- Resets a manifest back to packaged without incrementing upload_attempts.
-- Used for auth failures where the manifest itself is healthy and should
-- not burn a retry attempt.
update provenance_manifests set
    status = 'packaged',
    updated_at = ?
where manifest_id = ?;
