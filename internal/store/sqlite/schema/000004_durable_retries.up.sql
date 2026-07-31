-- Processing is represented by lease metadata on a pending checkpoint.
alter table checkpoints add column attempt_count integer not null default 0;
alter table checkpoints add column last_error text;
alter table checkpoints add column next_attempt_at integer not null default 0;
alter table checkpoints add column lease_owner text;
alter table checkpoints add column lease_until integer;
