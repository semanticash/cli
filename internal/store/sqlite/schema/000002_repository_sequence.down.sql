drop index if exists idx_checkpoints_repository_sequence;
alter table checkpoints drop column repository_sequence;
