-- name: UpsertRepoSession :exec
insert into implementation_repo_sessions (
    implementation_id, provider, provider_session_id,
    canonical_path, session_id, first_seen_at, last_seen_at
) values (?, ?, ?, ?, ?, ?, ?)
on conflict (implementation_id, canonical_path, session_id) do update
set last_seen_at = excluded.last_seen_at;

-- name: FindImplementationsByLocalSession :many
select i.* from implementations i
join implementation_repo_sessions rs on i.implementation_id = rs.implementation_id
where rs.session_id = ?
order by
    case i.state when 'active' then 0 when 'dormant' then 1 else 2 end asc,
    i.last_activity_at desc;

-- name: ListRepoSessionsForImplementation :many
select * from implementation_repo_sessions
where implementation_id = ?
order by first_seen_at asc;

-- name: MoveRepoSessions :exec
update implementation_repo_sessions
set implementation_id = sqlc.arg(target_id)
where implementation_id = sqlc.arg(source_id);

-- name: DeleteRepoSessionsByProviderSession :exec
delete from implementation_repo_sessions
where implementation_id = ?
  and provider = ?
  and provider_session_id = ?;
