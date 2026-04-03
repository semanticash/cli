-- Drop indexes first (safe even if they don't exist)

drop index if exists agent_events_session_ts_index;
drop index if exists agent_events_repo_ts_kind_index;

drop index if exists agent_sessions_repo_provider_index;
drop index if exists agent_sessions_repo_last_seen_index;
drop index if exists agent_sessions_parent_index;
drop index if exists agent_sessions_repo_provider_session_unique;

drop index if exists agent_sources_repo_provider_index;

drop index if exists session_checkpoints_checkpoint_index;

drop index if exists commit_links_checkpoint_index;
drop index if exists commit_links_repository_index;

drop index if exists checkpoints_repository_created_index;

-- Drop tables in FK-safe order (children first)

drop table if exists checkpoint_stats;
drop table if exists session_checkpoints;
drop table if exists agent_events;
drop table if exists agent_sessions;
drop table if exists agent_sources;
drop table if exists commit_links;
drop table if exists checkpoints;
drop table if exists repositories;
