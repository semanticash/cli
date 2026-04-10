-- Drop indexes first

drop index if exists idx_conflicts_unresolved;
drop index if exists idx_observations_failed;
drop index if exists idx_observations_pending;
drop index if exists idx_impl_commits_auto_unique;
drop index if exists idx_impl_commits_hash;
drop index if exists idx_impl_branches_lookup;
drop index if exists idx_repo_sessions_by_provider;
drop index if exists idx_repo_sessions_by_session_id;
drop index if exists idx_provider_sessions_lookup;
drop index if exists idx_impl_repos_path;
drop index if exists idx_impl_state;

-- Drop tables in FK-safe order (children first)

drop table if exists observation_conflicts;
drop table if exists observations;
drop table if exists implementation_commits;
drop table if exists implementation_branches;
drop table if exists implementation_repo_sessions;
drop table if exists implementation_provider_sessions;
drop table if exists implementation_repos;
drop table if exists implementations;
