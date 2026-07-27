drop index if exists idx_agent_events_insert_seq;
alter table agent_events drop column insert_seq;
alter table checkpoints drop column event_cursor;
