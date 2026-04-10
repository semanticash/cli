# Implementations

Detailed guide to Semantica's cross-repo implementation view.

---

## What this feature does

An implementation is Semantica's local record for one piece of work that
spreads across repositories.

In practice, that usually looks like:

- an API change in one repo
- a client or SDK update in another
- a UI, docs, or follow-up commit in a third

Semantica groups that activity into one implementation so the related repos,
sessions, commits, title, and story stay together instead of being scattered
across separate local repo logs.

---

## Commands

### Inspect implementations

```bash
semantica implementations
semantica implementations --json
semantica implementations --all
semantica implementations --include-single
semantica implementations --limit 50

semantica impl <implementation_id>
semantica impl <implementation_id> --json
semantica impl <implementation_id> --verbose
```

- `semantica implementations` lists the current implementation set.
- `semantica impl <implementation_id>` shows the implementation card with story,
  repos, commits, stats, and timeline details.
- `--all` includes older dormant implementations and single-repo work.
- `--include-single` includes single-repo implementations without widening the
  rest of the list.
- `--json` returns structured output for scripts, dashboards, and other tools.

### Suggest or apply titles and summaries

```bash
semantica suggest impl
semantica suggest impl --json

semantica suggest impl <implementation_id>
semantica suggest impl <implementation_id> --json
semantica suggest impl <implementation_id> --apply
```

- `semantica suggest impl` runs in batch mode. It suggests titles for untitled
  implementations and identifies possible merge candidates.
- `semantica suggest impl <implementation_id>` generates a title and summary for
  one implementation.
- `--apply` writes the suggested title and summary to the implementation.

### Manual controls

```bash
semantica implementations close <implementation_id>
semantica implementations link <implementation_id> --session <session_id>
semantica implementations link <implementation_id> --commit <sha> --repo /path/to/repo
semantica implementations merge <target_id> <source_id>
```

- `close` marks an implementation as finished and prevents future activity from
  attaching to it.
- `link` manually attaches a session or commit when you want to correct or
  refine grouping.
- `merge` combines two implementations when Semantica split work that should
  have stayed together.

---

## States and boundaries

Implementations move through three states:

- `active` - current work that is still receiving related activity
- `dormant` - older work that is not currently moving, but can still resume
- `closed` - finished work that will not receive new attachments

The practical boundary is:

- `active` and `dormant` implementations can still absorb later related work
  when Semantica sees a strong match
- `closed` implementations are final and are excluded from new attachment
  decisions

If you want later work to form a new implementation instead of resuming an
older one, close the older implementation first.

```bash
semantica impl close <implementation_id>
```

That keeps the existing implementation in history while forcing future work to
materialize separately.

---

## How work gets attached

Semantica uses deterministic attach rules to decide whether new activity
belongs to an existing implementation.

The strongest signals are:

- the same provider session identity
- parent-child provider session relationships
- active branch context in the related repos
- existing repo and commit links already attached to the implementation

This means Semantica is not trying to invent a story from scratch. It is
linking work from capture data it already recorded locally.

Some long-lived sessions and branch patterns are still ambiguous by nature.
That is why `link`, `merge`, and `close` exist as manual controls.

---

## Automatic titles and summaries

When `auto-implementation-summary` is enabled, Semantica generates titles and
summaries in the background for implementations that span multiple
repositories.

```bash
semantica set auto-implementation-summary enabled
semantica set auto-implementation-summary disabled
```

The worker only generates or refreshes the title and summary when the
implementation scope changes in a meaningful way.

Current behavior:

- single-repo implementations are skipped
- a newly multi-repo implementation gets a title and summary
- a summary can refresh when a new repo joins the implementation

Manual apply is still available when you want to regenerate on demand:

```bash
semantica suggest impl <implementation_id> --apply
```

---

## JSON for downstream services or agents

The implementation view is available as structured JSON so other local tools
can consume the same object Semantica shows in the terminal.

Useful commands:

```bash
semantica implementations --json
semantica impl <implementation_id> --json
semantica suggest impl --json
semantica suggest impl <implementation_id> --json
```

This is the best fit when you want to:

- feed implementation data into a dashboard
- build a local planner or automation on top of implementations
- hand implementation summaries to another agent or service

The CLI view and the JSON view are meant to describe the same implementation,
not two separate concepts.

---

## Caveats

- Implementations are local-first and depend on Semantica capture on the
  current machine.
- Cross-repo grouping only works for repositories that are enabled and visible
  to the broker on that machine.
- Suggestions are advisory. They do not rewrite implementation history unless
  you explicitly apply them.
- Automatic title and summary generation is best-effort and runs in the
  background.
