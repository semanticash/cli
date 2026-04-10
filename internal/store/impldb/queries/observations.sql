-- name: InsertObservation :exec
insert into observations (
    observation_id, provider, provider_session_id, parent_session_id,
    source_project_path, target_repo_path,
    event_ts, created_at, reconciled
) values (?, ?, ?, ?, ?, ?, ?, ?, 0);

-- name: ListPendingObservations :many
select * from observations
where reconciled = 0
order by
    case when parent_session_id is null or parent_session_id = '' then 0 else 1 end asc,
    created_at asc
limit ?;

-- name: ListDeferredObservations :many
select * from observations
where reconciled = 4
order by created_at asc
limit ?;

-- name: ListRetryableObservations :many
select * from observations
where reconciled = 3
  and reconcile_attempts < ?
order by created_at asc
limit ?;

-- name: MarkObservationReconciled :exec
update observations set reconciled = 1 where observation_id = ?;

-- name: MarkObservationConflict :exec
update observations set reconciled = 2 where observation_id = ?;

-- name: MarkObservationDeferred :exec
update observations
set reconciled = 4,
    reconcile_attempts = reconcile_attempts + 1
where observation_id = ?;

-- name: MarkObservationFailed :exec
update observations
set reconciled = 3,
    reconcile_attempts = reconcile_attempts + 1,
    last_error = ?
where observation_id = ?;

-- name: PruneReconciledObservations :execresult
delete from observations
where reconciled = 1
  and created_at < ?;

-- name: CountFailedObservations :one
select count(*) from observations
where reconciled = 3
  and reconcile_attempts >= ?;
