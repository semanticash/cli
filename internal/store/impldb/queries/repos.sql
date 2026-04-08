-- name: UpsertImplementationRepo :exec
insert into implementation_repos (
    implementation_id, canonical_path, display_name,
    repo_role, first_seen_at, last_seen_at
) values (?, ?, ?, ?, ?, ?)
on conflict (implementation_id, canonical_path) do update
set last_seen_at = excluded.last_seen_at,
    repo_role = case
        -- origin is the highest rank, never downgraded
        when implementation_repos.repo_role = 'origin' then 'origin'
        when excluded.repo_role = 'origin' then 'origin'
        -- downstream outranks related
        when implementation_repos.repo_role = 'downstream' then 'downstream'
        when excluded.repo_role = 'downstream' then 'downstream'
        -- otherwise keep the incoming role
        else excluded.repo_role
    end;

-- name: ListImplementationRepos :many
select * from implementation_repos
where implementation_id = ?
order by first_seen_at asc;

-- name: CountReposForImplementation :one
select count(distinct canonical_path) from implementation_repos
where implementation_id = ?;

-- name: DeleteReposForImplementation :exec
delete from implementation_repos where implementation_id = ?;

-- name: DeleteOrphanedRepos :exec
-- Remove repos that no longer have any repo-scoped data (sessions, commits,
-- or branches) in this implementation.
delete from implementation_repos
where implementation_repos.implementation_id = sqlc.arg(impl_id)
  and implementation_repos.canonical_path not in (
    select distinct rs.canonical_path
    from implementation_repo_sessions rs
    where rs.implementation_id = sqlc.arg(impl_id)
    union
    select distinct c.canonical_path
    from implementation_commits c
    where c.implementation_id = sqlc.arg(impl_id)
    union
    select distinct b.canonical_path
    from implementation_branches b
    where b.implementation_id = sqlc.arg(impl_id)
  );
