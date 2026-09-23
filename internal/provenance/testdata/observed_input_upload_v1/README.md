# observed-input upload fixture (v1)

Frozen shared contract for uploading Claude observed-input evidence. This copy is
canonical; the API repository holds a byte-identical mirror. `manifest.sha256`
freezes every contract file so both repositories verify the same bytes without
sharing a checkout. Any contract change must update both copies and their tests
together.

## Layout

- `local/evidence.json` — a per-turn observed-input document as persisted locally,
  referencing content objects by SHA-256.
- `local/content/<sha256>` — raw captured content objects (6).
- `outbound/observed_input.json` — the same document after the approved privacy
  transform, ready for upload.
- `outbound/content/<sha256>` — transformed content objects (5; the PDF is withheld).
- `manifest.sha256` — SHA-256 of every contract file above.

## What the transform does (local -> outbound)

- Text content (request instruction, delivered text, differing source text) is
  passed through secret redaction; `content_ref` / `source_content_ref` and their
  sizes are rewritten to the transformed objects. Identical bytes remain one shared
  content object with distinct delivery identities and links.
- PDF/binary content is withheld: its `content_ref` is dropped and a
  `representation.upload` marker `{ "state": "withheld", "reason":
  "binary_privacy_policy" }` is added. Captured `state`, `content_size`, and
  `media_type` are preserved. Withholding is never a capture gap; there is no
  outbound content object for withheld bytes.
- `reported_source_bytes` (the provider's measurement) is preserved; `content_size`
  describes the bytes actually referenced.
- Metadata is sanitized: `input_source.locator` (the named document) is made
  repo-relative when contained, else reduced to its basename; URL locators are
  stripped of credentials, query, and fragment. `source.locator` (the provider
  transcript) has its private directory removed while `native` and `position` are
  preserved. Sanitized locators are display metadata only and are never a merge key.
- Gap details are redacted; explicit gaps and unavailable sections survive without
  invented content references.
- `upload_transform_version` records the transform rules at the document top level.

## Cases covered

request instruction, text attachment, withheld PDF attachment, Read result with
differing source text, WebFetch summary, identical bytes delivered twice, secrets in
instruction / delivered-or-source text / URL / gap detail, and an explicit capture gap.
