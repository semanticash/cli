-- name: InsertCheckpoint :exec
-- Allocate sequence and event cursor in the insert transaction.
insert into checkpoints(
    checkpoint_id, repository_id, created_at, kind, trigger, message,
    manifest_hash, size_bytes, status, completed_at, repository_sequence,
    event_cursor
) values (sqlc.arg(checkpoint_id), sqlc.arg(repository_id), sqlc.arg(created_at),
    sqlc.arg(kind), sqlc.arg(trigger), sqlc.arg(message), sqlc.arg(manifest_hash),
    sqlc.arg(size_bytes), sqlc.arg(status), sqlc.arg(completed_at),
    (select coalesce(max(repository_sequence), 0) + 1
     from checkpoints c2 where c2.repository_id = sqlc.arg(repository_id)),
    (select coalesce(max(e.insert_seq), 0) from agent_events e));

-- name: GetCheckpointByID :one
select * from checkpoints where checkpoint_id = ?;

-- name: ListCheckpointsByRepository :many
select * from checkpoints where repository_id = ? order by created_at desc limit ?;

-- name: DeleteCheckpointByID :exec
delete from checkpoints where checkpoint_id = ?;

-- name: GetLatestCheckpointForRepo :one
select * from checkpoints where repository_id = ? order by repository_sequence desc limit 1;

-- name: CompleteCheckpoint :exec
update checkpoints
set manifest_hash = ?, size_bytes = ?, status = 'complete', completed_at = ?,
    lease_owner = null, lease_until = null, last_error = null, next_attempt_at = 0
where checkpoint_id = ?;

-- name: FailCheckpoint :execrows
-- Terminal failure: permanent error or attempts exhausted; the final
-- error is preserved for doctor and manual retry.
update checkpoints
set status = 'failed', completed_at = ?, last_error = ?,
    lease_owner = null, lease_until = null, next_attempt_at = 0
where checkpoint_id = ?;

-- name: ClaimCheckpoint :one
-- Atomic claim: takes a due pending checkpoint whose lease is free or
-- expired and increments attempt_count exactly once. A live lease on a
-- pending row represents processing. Returns no row when unavailable.
update checkpoints
set lease_owner = sqlc.arg(lease_owner),
    lease_until = sqlc.arg(lease_until),
    attempt_count = attempt_count + 1
where checkpoint_id = sqlc.arg(checkpoint_id)
  and status = 'pending'
  and next_attempt_at <= sqlc.arg(now)
  and (lease_owner is null or lease_until < sqlc.arg(now))
returning *;

-- name: ReleaseCheckpointForRetry :execrows
-- Transient failure: release the lease with a scheduled retry. The
-- attempt counter was already incremented by the claim. Callers must
-- require one affected row; zero means the lease was lost and the
-- transition was not recorded.
update checkpoints
set last_error = ?, next_attempt_at = ?,
    lease_owner = null, lease_until = null
where checkpoint_id = ? and lease_owner = ?;

-- name: RetryFailedCheckpoint :execrows
-- Manual retry: a human intervened, so the attempt budget resets.
update checkpoints
set status = 'pending', attempt_count = 0, next_attempt_at = 0,
    lease_owner = null, lease_until = null,
    last_error = null, completed_at = null
where checkpoint_id = ? and status = 'failed';

-- name: ListCheckpointsWithCommit :many
select c.checkpoint_id, c.created_at, c.kind, c.trigger, c.message,
       c.size_bytes, c.status, c.completed_at, cl.commit_hash, c.manifest_hash
from checkpoints c
    left join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
order by c.created_at desc limit ?;

-- name: GetPreviousCompletedCheckpoint :one
-- Sequence selects the predecessor; timestamps bound its event window.
select * from checkpoints
where repository_id = ?
  and status = 'complete'
  and manifest_hash is not null
  and repository_sequence < ?
order by repository_sequence desc
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
-- Ignore manual and baseline checkpoints when anchoring attribution.
select c.* from checkpoints c
    join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
  and c.status = 'complete'
  and c.repository_sequence < ?
order by c.repository_sequence desc
limit 1;

-- name: GetMostRecentCommitLinkedCheckpoint :one
-- Latest completed commit-linked checkpoint by repository sequence.
select c.* from checkpoints c
    join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
  and c.status = 'complete'
order by c.repository_sequence desc
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

-- name: ListPendingCommitLinkedCheckpoints :many
-- Include failed rows because a failed queue head blocks later work.
-- Newest commit links sort first for deterministic deduplication.
select c.checkpoint_id, c.repository_sequence, c.status, cl.commit_hash,
       c.attempt_count, c.last_error, c.next_attempt_at, c.lease_until
from checkpoints c
    join commit_links cl on cl.checkpoint_id = c.checkpoint_id
where c.repository_id = ?
  and c.status in ('pending', 'failed')
order by c.repository_sequence asc, cl.linked_at desc, cl.commit_hash desc;

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
