# Observed-input fixtures

Fixtures for the Claude observed-input normalizer.

Captured fixtures preserve specification text, PDF bytes, and original parent
links from Claude sessions. Paths are anonymized. Intermediate ancestors retain
only `uuid` and `parentUuid`; their bodies are omitted.

- `captured_pdf_attachment.jsonl` - prompt, PDF attachment, Read call, and result.
- `captured_text_attachment.jsonl` - prompt and text attachment with a line range.
- `captured_webfetch.jsonl` - prompt, WebFetch call, and returned summary with fetched byte count.

Synthetic fixtures use invented identities and relationships absent from the captures:

- `synthetic_multirequest.jsonl` - an attachment serialized after a later request.
- `synthetic_delegation.jsonl` - a delegated request with its own attachment.
- `synthetic_null_attachment.jsonl` - an attachment without a body linked to a request.
