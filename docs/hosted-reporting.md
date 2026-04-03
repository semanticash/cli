# Hosted Features

Semantica works fully offline by default. Local capture, attribution, checkpoints, rewind, and playbooks continue to work without any remote configuration.

If you want hosted features for a repo, authenticate once and then connect that repo:

```bash
semantica auth login
semantica connect
```

If the repo is already connected through a shared workspace, `semantica connect`
offers to request access from the workspace owner or admin. Until that request
is approved, Semantica keeps capturing locally and does not start hosted sync.

## What the CLI does

Semantica keeps provenance packaging local. Each completed turn can produce:

- a prompt blob when the turn has a captured prompt
- step provenance blobs for captured tool steps, and for transcript-replayed
  steps when the provider transcript has enough structured detail
- a provenance bundle that ties those blobs together

These artifacts are written under `.semantica/` first. If the repo is
connected, the background worker attempts a best-effort sync after each commit.
`semantica connect` also tries to sync a small initial batch of already
packaged turns and already-captured commit attribution so older history can
start draining right away.
Steps whose primary file is ignored by Git are filtered out during packaging,
so ignored-file provenance stays local and is not included in synced bundles.

`semantica disconnect` stops future sync attempts from the current repo:

```bash
semantica disconnect
```

## Shared workspaces

- `semantica connect` can prompt to request access when the repo already belongs
  to another workspace.
- Pending requests stay pending until a workspace owner or admin approves them.
- Owners and admins can review requests with:

```bash
semantica workspace requests
```

- In non-interactive environments, `semantica connect` does not create access
  requests automatically. Rerun it in an interactive terminal if access needs
  to be requested.

## What stays local

- Capture state and transcripts
- Checkpoints and blob storage under `.semantica/`
- Attribution and playbooks stored in `lineage.db`

Connecting a repo does not disable or replace any of the local workflows.

## Authentication

`semantica auth login` authenticates you globally. It does not connect any repos by itself.

You can also provide credentials through `SEMANTICA_API_KEY`, which is useful for CI or other non-interactive environments.

## Failure behavior

- Sync is best-effort.
- Failures are logged to `.semantica/worker.log`.
- Sync failures never block commits, checkpoints, or local CLI features.
- If some packaged turns cannot be synced, they stay local.

## Security and redaction

- Redaction applies only to outbound sync artifacts. Local raw capture and local blob storage under `.semantica/` are not rewritten.
- Semantica redacts likely secrets and normalizes known provenance path fields to repo-relative form where possible before sync.
- Redaction is best-effort, not a guarantee. Unknown secret formats, future provider fields, or provider-specific payload changes may still require updates.
- Even when credentials are redacted, synced prompts, command output, and edited content can still contain sensitive business context.

## Notes

- Hosted sync is optional. The CLI keeps packaging provenance locally even when the repo is not connected.
