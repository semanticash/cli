-- name: GetCheckpointCapture :one
select record_json from checkpoint_capture where checkpoint_id = ?;

-- name: SaveCheckpointCapture :exec
insert into checkpoint_capture(checkpoint_id, record_json) values (?, ?)
on conflict(checkpoint_id) do update set record_json = excluded.record_json;

-- name: ReleaseCheckpointForCapture :execrows
-- Capture waits release the claim without spending a processing attempt.
update checkpoints set
    last_error = ?, next_attempt_at = ?, attempt_count = max(0, attempt_count - 1),
    lease_owner = null, lease_until = null
where checkpoint_id = ? and lease_owner = ? and status = 'pending';
