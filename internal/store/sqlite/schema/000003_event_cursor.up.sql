-- event_cursor snapshots insert order for equal-timestamp boundaries.
-- Legacy null boundaries use timestamps alone.
alter table checkpoints add column event_cursor integer;

-- Monotonic event arrival order, backfilled from rowid.
alter table agent_events add column insert_seq integer;
update agent_events set insert_seq = rowid;
create unique index idx_agent_events_insert_seq on agent_events(insert_seq);
