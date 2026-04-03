-- name: InsertRepository :exec
insert into repositories(repository_id, root_path, created_at, enabled_at)
values (?, ?, ?, ?);

-- name: GetRepositoryByRootPath :one
select * from repositories where root_path = ?;

-- name: GetRepositoryByID :one
select * from repositories where repository_id = ?;

-- name: UpdateRepositoryEnabledAt :exec
update repositories set enabled_at = ? where repository_id = ?;

-- name: ListRepositories :many
select * from repositories order by created_at desc;