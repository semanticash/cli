
create table if not exists repositories (
    repository_id text primary key , -- stable UUID
    root_path text not null unique , -- absolute repository_path
    created_at integer not null , -- unix milliseconds
    enabled_at integer not null default 0 -- unix milliseconds; when semantica was enabled
);

create table if not exists checkpoints (
    checkpoint_id text primary key , -- checkpoint UUID
    repository_id text not null ,
    created_at integer not null , -- unix milliseconds
    kind text not null check(kind in ('manual', 'auto', 'baseline')) , -- manual, auto, baseline
    trigger text , -- agent_step, pre_commit, rewind
    message text , -- user message (manual) or generated
    manifest_hash text , -- CAS pointer, let pre-commit be fast and the worker fill the details later
    size_bytes integer , -- optional (future stats)
    status text not null default 'complete' check(status in ('pending', 'complete', 'failed')) , -- support lightweight async checkpoints
    completed_at integer , -- null until worker finishes; unix milliseconds
    summary_json text , -- JSON: LLM-generated playbook (title, intent, outcome, learnings, friction, open_items)
    summary_model text , -- which model produced the playbook (e.g. "sonnet")

    foreign key (repository_id) references repositories(repository_id) on delete cascade
);
create index if not exists checkpoints_repository_created_index on checkpoints(repository_id, created_at desc);

create table if not exists commit_links (
    commit_hash text primary key , -- git commit SHA
    repository_id text not null ,
    checkpoint_id text not null ,
    linked_at integer not null , -- when linkage happened for debugging, audits, ordering

    foreign key (repository_id) references repositories(repository_id) on delete cascade,
    foreign key (checkpoint_id) references checkpoints(checkpoint_id) on delete cascade
);
create index if not exists commit_links_repository_index on commit_links(repository_id);
create index if not exists commit_links_checkpoint_index on commit_links(checkpoint_id);

-- Agent sources: where to read from + how far we read
create table if not exists agent_sources (
    source_id text primary key ,
    repository_id text not null ,
    provider text not null , -- claude_code, cursor, codex, gemini
    source_key text not null , -- stable per-provider key (e.g. file path, db path, workspace id)
    last_seen_at integer not null , -- unix milliseconds
    created_at integer not null , -- unix milliseconds

    foreign key (repository_id) references repositories(repository_id) on delete cascade ,
    unique (repository_id, provider, source_key)
);
create index if not exists agent_sources_repo_provider_index on agent_sources(repository_id, provider);

-- Agent sessions: logical conversation/session
create table if not exists agent_sessions (
    session_id text primary key , -- Semantica UUID
    provider_session_id text not null , -- e.g. Claude sessionID, Cursor chat id, etc
    parent_session_id text , -- for sub-agents / task spawns
    repository_id text not null ,
    provider text not null ,
    source_id text not null , -- agent_sources.source_id
    started_at integer not null ,
    last_seen_at integer not null ,
    metadata_json text not null , -- provider metadata (model, project etc.)
    source_repo_path text , -- canonical path of repo where session was launched (cross-repo routing)
    model text , -- LLM model name (e.g. "opus 4.6", "gemini-2.5-pro"); NULL = unknown

    foreign key (repository_id) references repositories(repository_id) on delete cascade ,
    foreign key (source_id) references agent_sources(source_id) on delete cascade ,
    foreign key (parent_session_id) references agent_sessions(session_id) on delete set null
);
create unique index if not exists agent_sessions_repo_provider_session_unique on agent_sessions (repository_id, provider, provider_session_id);
create index if not exists agent_sessions_parent_index on agent_sessions(parent_session_id);
create index if not exists agent_sessions_repo_last_seen_index on agent_sessions(repository_id, last_seen_at desc);
create index if not exists agent_sessions_repo_provider_index on agent_sessions(repository_id, provider);

-- Agent events: append-only, raw payload
create table if not exists agent_events (
    event_id text primary key , -- stable idempotent key (e.g. sha256(source_key + offset + line))
    session_id text not null ,
    repository_id text not null ,
    ts integer not null , -- unix milliseconds (best-effort)
    kind text not null , -- e.g. user, assistant, tool, system, error, unknown
    payload_hash text , -- CAS pointer to raw json line
    role text , -- user/assistant/tool/system
    tool_uses text , -- JSON: {"content_types":["text","tool_use"],"tools":[{"name":"Edit","file_path":"/foo.go","file_op":"edit"}]}
    tokens_in integer ,
    tokens_out integer ,
    tokens_cache_read integer ,
    tokens_cache_create integer ,
    summary text , -- short extracted text for lists
    provider_event_id text , -- optional: stable id from provider if it exists
    turn_id text , -- links event to the turn that produced it
    tool_use_id text , -- stable provider tool call id (e.g. Claude toolu_*)
    tool_name text , -- Write, Edit, Bash, Agent, etc.
    event_source text not null default 'transcript' , -- hook or transcript
    provenance_hash text , -- CAS pointer to raw hook payload for backend provenance reconstruction

    foreign key (session_id) references agent_sessions(session_id) on delete cascade,
    foreign key (repository_id) references repositories(repository_id) on delete cascade
);
create index if not exists agent_events_repo_ts_kind_index on agent_events(repository_id, ts desc, kind);
create index if not exists agent_events_session_ts_index on agent_events(session_id, ts desc);
create index if not exists agent_events_dedup_index on agent_events(turn_id, tool_use_id, tool_name);

-- Provenance manifests: per-turn packaging state for backend upload.
create table if not exists provenance_manifests (
    manifest_id text primary key ,
    repository_id text not null ,
    session_id text not null ,
    turn_id text not null ,
    provider text not null ,
    kind text not null check(kind in ('turn_bundle')) ,
    transcript_ref text , -- local provider transcript path (used by tidy and capture routing)
    provenance_bundle_hash text , -- CAS hash of per-turn provenance bundle JSON
    started_at integer not null ,
    completed_at integer ,
    status text not null default 'pending' check(status in ('pending', 'packaged', 'uploading', 'uploaded', 'failed')) ,
    upload_attempts integer not null default 0 ,
    last_error text ,
    upload_transform_version integer , -- version of redactForUpload used to derive upload hashes
    remote_verified_at integer , -- unix ms when backend confirmed receipt
    created_at integer not null ,
    updated_at integer not null ,

    foreign key (repository_id) references repositories(repository_id) on delete cascade ,
    foreign key (session_id) references agent_sessions(session_id) on delete cascade ,
    unique (repository_id, session_id, turn_id, kind)
);
create index if not exists provenance_manifests_status_created_index on provenance_manifests(status, created_at);
create index if not exists provenance_manifests_repo_created_index on provenance_manifests(repository_id, created_at desc);


create table if not exists session_checkpoints (
    session_id text not null ,
    checkpoint_id text not null ,

    primary key (session_id, checkpoint_id) ,
    foreign key (session_id) references agent_sessions(session_id) on delete cascade ,
    foreign key (checkpoint_id) references checkpoints(checkpoint_id) on delete cascade
);
create index if not exists session_checkpoints_checkpoint_index on session_checkpoints(checkpoint_id);

-- Makes list/checkpoint page fast and keeps SQLite small
create table if not exists checkpoint_stats (
    checkpoint_id text primary key ,
    session_count integer not null default 0 ,
    files_changed integer not null default 0 ,
    ai_percentage real not null default -1 , -- -1 = not computed; 0-100 = AI attribution %

    foreign key (checkpoint_id) references checkpoints(checkpoint_id) on delete cascade
);

-- Tracks attribution backfill replay state per connected repo binding.
-- One row per connected_repo_id: cursor advances through historical
-- commit_links oldest-first until the cutoff is reached.
create table if not exists remote_attribution_backfills (
    connected_repo_id text primary key ,
    repository_id text not null ,
    cutoff_linked_at integer not null ,
    cutoff_commit_hash text not null ,
    cursor_linked_at integer not null default 0 ,
    cursor_commit_hash text not null default '' ,
    status text not null default 'pending' check(status in ('pending', 'complete')) ,
    failed_commit_hash text ,
    retry_attempts integer not null default 0 ,
    last_error text ,
    updated_at integer not null ,

    foreign key (repository_id) references repositories(repository_id) on delete cascade
);
create index if not exists remote_attribution_backfills_repo_status_index
on remote_attribution_backfills(repository_id, status);

