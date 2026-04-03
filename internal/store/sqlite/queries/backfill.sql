-- name: GetAttributionBackfill :one
select * from remote_attribution_backfills where connected_repo_id = ?;

-- name: UpsertAttributionBackfill :exec
-- Initializes replay state for a connected repo binding. If the row already
-- exists, extends the cutoff if the new cutoff is newer (never shrinks).
insert into remote_attribution_backfills (
    connected_repo_id, repository_id,
    cutoff_linked_at, cutoff_commit_hash,
    cursor_linked_at, cursor_commit_hash,
    status, updated_at
) values (?, ?, ?, ?, 0, '', 'pending', ?)
on conflict(connected_repo_id) do update set
    cutoff_linked_at = case
        when excluded.cutoff_linked_at > remote_attribution_backfills.cutoff_linked_at
            or (excluded.cutoff_linked_at = remote_attribution_backfills.cutoff_linked_at
                and excluded.cutoff_commit_hash > remote_attribution_backfills.cutoff_commit_hash)
        then excluded.cutoff_linked_at
        else remote_attribution_backfills.cutoff_linked_at
    end,
    cutoff_commit_hash = case
        when excluded.cutoff_linked_at > remote_attribution_backfills.cutoff_linked_at
            or (excluded.cutoff_linked_at = remote_attribution_backfills.cutoff_linked_at
                and excluded.cutoff_commit_hash > remote_attribution_backfills.cutoff_commit_hash)
        then excluded.cutoff_commit_hash
        else remote_attribution_backfills.cutoff_commit_hash
    end,
    status = case
        when remote_attribution_backfills.status = 'complete'
            and (excluded.cutoff_linked_at > remote_attribution_backfills.cutoff_linked_at
                or (excluded.cutoff_linked_at = remote_attribution_backfills.cutoff_linked_at
                    and excluded.cutoff_commit_hash > remote_attribution_backfills.cutoff_commit_hash))
        then 'pending'
        else remote_attribution_backfills.status
    end,
    updated_at = excluded.updated_at;

-- name: AdvanceBackfillCursor :exec
-- Advances the cursor past a successfully pushed or skipped commit.
-- Also clears any failure state since the head-of-line commit moved.
update remote_attribution_backfills set
    cursor_linked_at = ?,
    cursor_commit_hash = ?,
    failed_commit_hash = null,
    retry_attempts = 0,
    last_error = null,
    updated_at = ?
where connected_repo_id = ?;

-- name: CompleteBackfill :exec
update remote_attribution_backfills set
    status = 'complete',
    failed_commit_hash = null,
    retry_attempts = 0,
    last_error = null,
    updated_at = ?
where connected_repo_id = ?;

-- name: RecordBackfillFailure :exec
-- Records a retryable failure on the current head-of-line commit.
update remote_attribution_backfills set
    failed_commit_hash = ?,
    retry_attempts = retry_attempts + 1,
    last_error = ?,
    updated_at = ?
where connected_repo_id = ?;

-- name: ListBackfillReplayCandidates :many
-- Returns the next batch of commit links eligible for replay, ordered
-- oldest-first. Uses (linked_at, commit_hash) tuple comparison for
-- deterministic cursor-based pagination.
select cl.commit_hash, cl.checkpoint_id, cl.linked_at
from commit_links cl
where cl.repository_id = sqlc.arg(repository_id)
  and (
    cl.linked_at > sqlc.arg(cursor_linked_at)
    or (cl.linked_at = sqlc.arg(cursor_linked_at) and cl.commit_hash > sqlc.arg(cursor_commit_hash))
  )
  and (
    cl.linked_at < sqlc.arg(cutoff_linked_at)
    or (cl.linked_at = sqlc.arg(cutoff_linked_at) and cl.commit_hash <= sqlc.arg(cutoff_commit_hash))
  )
order by cl.linked_at asc, cl.commit_hash asc
limit sqlc.arg(batch_limit);

-- name: GetLatestCommitLink :one
-- Returns the most recent commit link for a repository. Used to snapshot
-- the backfill cutoff at connect time.
select commit_hash, linked_at
from commit_links
where repository_id = ?
order by linked_at desc, commit_hash desc
limit 1;
