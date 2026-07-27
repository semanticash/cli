-- repository_sequence is the ordering authority for each repository.
alter table checkpoints add column repository_sequence integer not null default 0;

-- Historical ordering is best-effort when timestamps are equal.
update checkpoints set repository_sequence = (
    select count(*) from checkpoints c2
    where c2.repository_id = checkpoints.repository_id
      and (c2.created_at < checkpoints.created_at
           or (c2.created_at = checkpoints.created_at
               and c2.checkpoint_id <= checkpoints.checkpoint_id))
);

create unique index idx_checkpoints_repository_sequence
    on checkpoints(repository_id, repository_sequence);
