-- Completion markers distinguish successful empty attribution from a stage
-- that never ran and record successful hosted pushes. Historical nulls remain
-- unknown.
alter table checkpoint_stats add column attribution_computed_at integer;
alter table checkpoint_stats add column attribution_pushed_at integer;
