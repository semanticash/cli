-- name: InsertCommitLink :exec
insert or ignore into commit_links(commit_hash, repository_id, checkpoint_id, linked_at)
values (?, ?, ?, ?);

-- name: GetCommitLinkByCommitHash :one
select * from commit_links where commit_hash = ?;

-- name: GetCommitLinksByCheckpoint :many
select * from commit_links where checkpoint_id = ?;

-- name: ResolveCommitLinkByPrefix :many
select commit_hash from commit_links
where commit_hash like ? and repository_id = ?
limit 2;

-- name: ListCommitLinksByRepository :many
select * from commit_links where repository_id = ? order by linked_at desc limit ?;

-- name: ListUserPromptsForCommit :many
-- Returns user prompts attributable to a single commit.
--
-- Isolation rules:
--   * User prompt events only; assistant and tool-result events are excluded.
--   * Events must fall within the commit checkpoint window.
--   * Duplicate turn IDs keep the earliest event for deterministic ordering.
--   * Events without a turn ID are excluded because they cannot be cited.
with this_checkpoint as (
    select c.checkpoint_id, c.repository_id, ck.created_at
    from commit_links c
    join checkpoints ck on ck.checkpoint_id = c.checkpoint_id
    where c.commit_hash = ?
    limit 1
),
prev_checkpoint_ts as (
    select coalesce(max(ck2.created_at), 0) as cutoff
    from commit_links c2
    join checkpoints ck2 on ck2.checkpoint_id = c2.checkpoint_id
    join this_checkpoint tc on tc.repository_id = c2.repository_id
    where ck2.created_at < tc.created_at
),
ranked as (
    select
        e.event_id, e.turn_id, e.ts, e.summary, e.payload_hash,
        row_number() over (partition by e.turn_id order by e.ts asc) as rn
    from this_checkpoint tc
    cross join prev_checkpoint_ts pc
    join session_checkpoints sc on sc.checkpoint_id = tc.checkpoint_id
    join agent_events e on e.session_id = sc.session_id
    where e.role = 'user'
      and e.kind = 'user'
      and e.turn_id is not null
      and e.turn_id != ''
      and e.ts <= tc.created_at
      and e.ts > pc.cutoff
)
select event_id, turn_id, ts, summary, payload_hash
from ranked
where rn = 1
order by ts asc;

-- name: ListAgentActionsForCommit :many
-- Returns assistant tool-use events attributable to a single commit.
--
-- Isolation rules mirror the user-prompt query:
--   * Assistant events with a non-empty tool_uses payload.
--   * Events must fall within the commit checkpoint window.
--   * Events without a turn ID or event ID are excluded.
--   * Each event has its own event_id so dedup is not required;
--     callers may emit multiple actions per event.
--   * Results are ordered oldest first so callers can drop the
--     prefix when applying a most-recent retention cap.
with this_checkpoint as (
    select c.checkpoint_id, c.repository_id, ck.created_at
    from commit_links c
    join checkpoints ck on ck.checkpoint_id = c.checkpoint_id
    where c.commit_hash = ?
    limit 1
),
prev_checkpoint_ts as (
    select coalesce(max(ck2.created_at), 0) as cutoff
    from commit_links c2
    join checkpoints ck2 on ck2.checkpoint_id = c2.checkpoint_id
    join this_checkpoint tc on tc.repository_id = c2.repository_id
    where ck2.created_at < tc.created_at
)
select
    e.event_id,
    s.provider,
    e.turn_id,
    sc.checkpoint_id,
    e.ts,
    e.tool_uses,
    e.payload_hash
from this_checkpoint tc
cross join prev_checkpoint_ts pc
join session_checkpoints sc on sc.checkpoint_id = tc.checkpoint_id
join agent_events e on e.session_id = sc.session_id
join agent_sessions s on s.session_id = e.session_id
where e.role = 'assistant'
  and e.tool_uses is not null
  and e.tool_uses != ''
  and e.turn_id is not null
  and e.turn_id != ''
  and e.event_id is not null
  and e.event_id != ''
  and e.ts <= tc.created_at
  and e.ts > pc.cutoff
order by e.ts asc;
