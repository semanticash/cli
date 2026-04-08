-- name: InsertConflict :exec
insert into observation_conflicts (
    conflict_id, observation_id, candidate_a, candidate_b,
    rule_name, resolved, created_at
) values (?, ?, ?, ?, ?, 0, ?);

-- name: ListUnresolvedConflicts :many
select * from observation_conflicts
where resolved = 0
order by created_at asc
limit ?;

-- name: ResolveConflict :exec
update observation_conflicts
set resolved = 1, resolution = ?
where conflict_id = ?;
