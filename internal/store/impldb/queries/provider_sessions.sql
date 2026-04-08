-- name: InsertProviderSession :exec
insert into implementation_provider_sessions (
    implementation_id, provider, provider_session_id,
    source_project_path, attach_rule, attached_at
) values (?, ?, ?, ?, ?, ?);

-- name: FindImplementationByProviderSession :one
select i.* from implementations i
join implementation_provider_sessions s on i.implementation_id = s.implementation_id
where s.provider = ? and s.provider_session_id = ?
  and i.state != 'closed'
order by i.last_activity_at desc
limit 1;

-- name: GetProviderSessionOwner :one
select s.implementation_id from implementation_provider_sessions s
join implementations i on i.implementation_id = s.implementation_id
where s.provider = ? and s.provider_session_id = ?
  and i.state != 'closed'
order by i.last_activity_at desc
limit 1;

-- name: ListProviderSessionsForImplementation :many
select * from implementation_provider_sessions
where implementation_id = ?
order by attached_at asc;
