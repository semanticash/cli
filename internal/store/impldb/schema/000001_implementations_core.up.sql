
create table if not exists implementations (
    implementation_id text primary key ,
    title text ,                                   -- user-set or LLM-suggested, nullable
    state text not null default 'active'
        check(state in ('active', 'dormant', 'closed')) ,
    created_at integer not null ,                   -- unix ms
    last_activity_at integer not null ,              -- unix ms, updated on every attach
    closed_at integer ,                              -- unix ms, set on close
    metadata_json text                               -- extensible: summary, tags, etc.
);
create index if not exists idx_impl_state on implementations(state, last_activity_at desc);

create table if not exists implementation_repos (
    implementation_id text not null references implementations(implementation_id) ,
    canonical_path text not null ,
    display_name text not null ,                     -- short repo name (basename of path)
    repo_role text not null default 'related'
        check(repo_role in ('origin', 'downstream', 'related')) ,
    first_seen_at integer not null ,
    last_seen_at integer not null ,
    primary key (implementation_id, canonical_path)
);
create index if not exists idx_impl_repos_path on implementation_repos(canonical_path);

-- A provider session can belong to multiple implementations over time
-- (e.g., after an implementation is closed and the session continues).
-- At most one of those implementations should be active/dormant at a time;
-- enforced by application logic in the reconciler, not schema constraint.
create table if not exists implementation_provider_sessions (
    implementation_id text not null references implementations(implementation_id) ,
    provider text not null ,
    provider_session_id text not null ,
    source_project_path text ,                       -- raw SourceProjectPath from hook event
    attach_rule text not null ,                      -- which rule caused the attach
    attached_at integer not null ,
    primary key (implementation_id, provider, provider_session_id)
);
create index if not exists idx_provider_sessions_lookup
    on implementation_provider_sessions(provider, provider_session_id);

create table if not exists implementation_repo_sessions (
    implementation_id text not null references implementations(implementation_id) ,
    provider text not null ,
    provider_session_id text not null ,
    canonical_path text not null ,                   -- which repo
    session_id text not null ,                       -- repo-local Semantica session UUID
    first_seen_at integer not null ,
    last_seen_at integer not null ,
    primary key (implementation_id, canonical_path, session_id)
);
create index if not exists idx_repo_sessions_by_session_id
    on implementation_repo_sessions(session_id);
create index if not exists idx_repo_sessions_by_provider
    on implementation_repo_sessions(provider, provider_session_id);

create table if not exists implementation_branches (
    implementation_id text not null references implementations(implementation_id) ,
    canonical_path text not null ,                   -- which repo
    branch text not null ,
    first_seen_at integer not null ,
    last_seen_at integer not null ,
    primary key (implementation_id, canonical_path, branch)
);
create index if not exists idx_impl_branches_lookup
    on implementation_branches(canonical_path, branch);

create table if not exists implementation_commits (
    implementation_id text not null references implementations(implementation_id) ,
    canonical_path text not null ,                   -- which repo
    commit_hash text not null ,
    attached_at integer not null ,
    attach_rule text not null ,
    primary key (implementation_id, canonical_path, commit_hash)
);
create index if not exists idx_impl_commits_hash on implementation_commits(commit_hash);
-- One commit belongs to one implementation for automatic rules.
-- Only explicit_link bypasses this constraint.
create unique index if not exists idx_impl_commits_auto_unique
    on implementation_commits(canonical_path, commit_hash)
    where attach_rule != 'explicit_link';

create table if not exists observations (
    observation_id text primary key ,                -- UUID
    provider text not null ,
    provider_session_id text not null ,
    parent_session_id text ,                         -- provider-level parent, nullable
    source_project_path text ,                       -- raw SourceProjectPath from RawEvent
    target_repo_path text not null ,                 -- where events were routed
    event_ts integer not null ,                      -- unix ms
    created_at integer not null ,                    -- unix ms, when observation was written
    reconciled integer not null default 0 ,          -- 0=pending, 1=processed, 2=conflict, 3=failed, 4=deferred
    reconcile_attempts integer not null default 0 ,
    last_error text
);
create index if not exists idx_observations_pending
    on observations(reconciled, created_at)
    where reconciled in (0, 4);
create index if not exists idx_observations_failed
    on observations(reconciled, reconcile_attempts)
    where reconciled = 3;

create table if not exists observation_conflicts (
    conflict_id text primary key ,                   -- UUID
    observation_id text not null references observations(observation_id) ,
    candidate_a text not null ,                      -- implementation_id
    candidate_b text not null ,                      -- implementation_id
    rule_name text not null ,                        -- which rule produced the conflict
    resolved integer not null default 0 ,
    resolution text ,                                -- 'merge', 'pick_a', 'pick_b', null if unresolved
    created_at integer not null
);
create index if not exists idx_conflicts_unresolved
    on observation_conflicts(resolved)
    where resolved = 0;
