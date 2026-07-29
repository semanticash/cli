alter table checkpoints drop column lease_until;
alter table checkpoints drop column lease_owner;
alter table checkpoints drop column next_attempt_at;
alter table checkpoints drop column last_error;
alter table checkpoints drop column attempt_count;
