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
