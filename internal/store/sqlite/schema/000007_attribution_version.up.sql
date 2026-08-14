-- NULL identifies results written before algorithm versions were recorded.
alter table checkpoint_stats add column attribution_version text;
