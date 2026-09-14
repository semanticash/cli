create table checkpoint_capture (
    checkpoint_id text primary key references checkpoints(checkpoint_id) on delete cascade,
    record_json text not null
);
