-- Adds final-response metadata to provenance manifests.
-- Nullable fields distinguish absent values from empty responses.
alter table provenance_manifests add column response_event_id text;
alter table provenance_manifests add column response_hash text;
alter table provenance_manifests add column response_summary text;
alter table provenance_manifests add column response_status text;
alter table provenance_manifests add column response_completed_at integer;
