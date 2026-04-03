-- name: InsertCheckpoint :exec
insert into checkpoints(
    checkpoint_id, repository_id, created_at, kind, trigger, message,
    manifest_hash, size_bytes, status, completed_at
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCheckpointByID :one
select * from checkpoints where checkpoint_id = ?;

-- name: ListCheckpointsByRepository :many
select * from checkpoints where repository_id = ? order by created_at desc limit ?;

-- name: DeleteCheckpointByID :exec
delete from checkpoints where checkpoint_id = ?;

-- name: GetLatestCheckpointForRepo :one
select * from checkpoints where repository_id = ? order by created_at desc limit 1;

-- name: CompleteCheckpoint :exec
update checkpoints
set manifest_hash = ?, size_bytes = ?, status = 'complete', completed_at = ?
where checkpoint_id = ?;

-- name: FailCheckpoint :exec
update checkpoints
set status = 'failed', completed_at = ?
where checkpoint_id = ?;

-- name: ListCheckpointsWithCommit :many
select c.checkpoint_id, c.created_at, c.kind, c.trigger, c.message,
       c.size_bytes, c.status, c.completed_at, cl.commit_hash, c.manifest_hash
from checkpoints c
    left join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
order by c.created_at desc limit ?;

-- name: GetPreviousCompletedCheckpoint :one
select * from checkpoints
where repository_id = ?
  and status = 'complete'
  and manifest_hash is not null
  and created_at < ?
order by created_at desc
limit 1;

-- name: UpsertCheckpointStats :exec
insert into checkpoint_stats (
    checkpoint_id, session_count, files_changed
) values (?, ?, ?)
on conflict(checkpoint_id) do update set
    session_count=excluded.session_count,
    files_changed=excluded.files_changed;

-- name: GetCheckpointStats :one
select * from checkpoint_stats where checkpoint_id = ?;

-- name: UpdateCheckpointAIPercentage :exec
update checkpoint_stats set ai_percentage = ? where checkpoint_id = ?;

-- name: CountSessionsForCheckpoint :one
select count(*) from session_checkpoints where checkpoint_id = ?;

-- name: GetPreviousCommitLinkedCheckpoint :one
-- Returns the most recent completed checkpoint before the given timestamp
-- that has an associated commit link. Used by attribution to anchor the
-- delta window to the previous commit rather than an intermediate manual
-- or baseline checkpoint.
select c.* from checkpoints c
    join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
  and c.status = 'complete'
  and c.created_at < ?
order by c.created_at desc
limit 1;

-- name: ResolveCheckpointByPrefix :many
select checkpoint_id from checkpoints
where checkpoint_id like ? and repository_id = ?
limit 2;

-- name: GetCheckpointSummary :one
select checkpoint_id, summary_json, summary_model from checkpoints
where checkpoint_id = ? and summary_json is not null;

-- name: ListStalePendingCheckpoints :many
-- Returns pending checkpoints older than the given threshold that have
-- no manifest and no commit link. Used by tidy to mark abandoned checkpoints
-- as failed.
select c.checkpoint_id, c.created_at from checkpoints c
    left join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
  and c.status = 'pending'
  and c.manifest_hash is null
  and cl.commit_hash is null
  and c.created_at < sqlc.arg(before_ts);

-- name: SaveCheckpointSummary :exec
update checkpoints
set summary_json = ?, summary_model = ?
where checkpoint_id = ?;

-- name: CountCheckpointsWithSummary :one
select cast(count(*) as integer) from checkpoints
where repository_id = ? and status = 'complete' and summary_json is not null;

-- name: ListRecentAIPercentages :many
select cs.checkpoint_id, cs.ai_percentage, c.created_at, cl.commit_hash
from checkpoint_stats cs
    join checkpoints c on c.checkpoint_id = cs.checkpoint_id
    join commit_links cl on cl.checkpoint_id = cs.checkpoint_id
where c.repository_id = ? and cs.ai_percentage >= 0
order by c.created_at desc
limit ?;
