-- name: UpsertImplementationBranch :exec
insert into implementation_branches (
    implementation_id, canonical_path, branch, first_seen_at, last_seen_at
) values (?, ?, ?, ?, ?)
on conflict (implementation_id, canonical_path, branch) do update
set last_seen_at = excluded.last_seen_at;

-- name: FindActiveImplementationByBranch :one
select i.* from implementations i
join implementation_branches b on i.implementation_id = b.implementation_id
where b.canonical_path = ?
  and b.branch = ?
  and i.state = 'active'
order by i.last_activity_at desc
limit 1;

-- name: ListBranchesForImplementation :many
select * from implementation_branches
where implementation_id = ?
order by first_seen_at asc;

-- name: DeleteBranchesForImplementation :exec
delete from implementation_branches where implementation_id = ?;
