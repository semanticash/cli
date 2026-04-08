-- name: UpsertImplementationRepo :exec
insert into implementation_repos (
    implementation_id, canonical_path, display_name,
    repo_role, first_seen_at, last_seen_at
) values (?, ?, ?, ?, ?, ?)
on conflict (implementation_id, canonical_path) do update
set last_seen_at = excluded.last_seen_at,
    repo_role = case
        when implementation_repos.repo_role = 'origin' then 'origin'
        when excluded.repo_role = 'origin' then 'origin'
        else excluded.repo_role
    end;

-- name: ListImplementationRepos :many
select * from implementation_repos
where implementation_id = ?
order by first_seen_at asc;

-- name: CountReposForImplementation :one
select count(distinct canonical_path) from implementation_repos
where implementation_id = ?;
