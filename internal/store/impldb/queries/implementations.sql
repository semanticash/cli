-- name: InsertImplementation :exec
insert into implementations (implementation_id, state, created_at, last_activity_at)
values (?, 'active', ?, ?);

-- name: GetImplementation :one
select * from implementations where implementation_id = ?;

-- name: ListImplementationsByState :many
select i.*,
    (select count(distinct canonical_path) from implementation_repos where implementation_id = i.implementation_id) as repo_count,
    (select count(*) from implementation_commits where implementation_id = i.implementation_id) as commit_count
from implementations i
where i.state in (sqlc.slice('states'))
order by i.last_activity_at desc
limit ?;

-- name: ListAllImplementations :many
select i.*,
    (select count(distinct canonical_path) from implementation_repos where implementation_id = i.implementation_id) as repo_count,
    (select count(*) from implementation_commits where implementation_id = i.implementation_id) as commit_count
from implementations i
order by i.last_activity_at desc
limit ?;

-- name: ListActiveOrMultiRepo :many
select i.*,
    (select count(distinct canonical_path) from implementation_repos where implementation_id = i.implementation_id) as repo_count,
    (select count(*) from implementation_commits where implementation_id = i.implementation_id) as commit_count
from implementations i
where i.state = 'active'
   or (select count(distinct canonical_path) from implementation_repos
       where implementation_id = i.implementation_id) > 1
order by i.last_activity_at desc
limit ?;

-- name: UpdateImplementationState :exec
update implementations set state = ?, closed_at = ? where implementation_id = ?;

-- name: UpdateImplementationActivity :exec
update implementations set last_activity_at = ? where implementation_id = ?;

-- name: UpdateImplementationTitle :exec
update implementations set title = ? where implementation_id = ?;

-- name: MarkDormant :execresult
update implementations
set state = 'dormant'
where state = 'active'
  and last_activity_at < ?;

-- name: ResolveImplementationByPrefix :many
select implementation_id from implementations
where implementation_id like sqlc.arg(prefix) || '%'
limit 10;

-- name: CountImplementationsByState :one
select count(*) from implementations
where state in (sqlc.slice('states'));

-- name: ListStaleImplementations :many
select * from implementations
where state = 'dormant'
  and last_activity_at < ?
order by last_activity_at asc;
