-- name: InsertImplementationCommit :exec
insert or ignore into implementation_commits (
    implementation_id, canonical_path, commit_hash,
    attached_at, attach_rule
) values (?, ?, ?, ?, ?);

-- name: FindImplementationByCommit :one
select implementation_id from implementation_commits
where canonical_path = ? and commit_hash = ?
limit 1;

-- name: ListImplementationCommits :many
select * from implementation_commits
where implementation_id = ?
order by attached_at asc;

-- name: CountCommitsForImplementation :one
select count(*) from implementation_commits
where implementation_id = ?;

-- name: MoveCommits :exec
update implementation_commits
set implementation_id = sqlc.arg(target_id)
where implementation_id = sqlc.arg(source_id);
