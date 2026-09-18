# Observed-input fixtures

Fixtures for the Claude observed-input normalizer.

Captured fixtures preserve provider structures and parent relationships from Claude sessions. Paths are anonymized. Intermediate ancestors retain only `uuid` and `parentUuid`; their bodies are omitted.

- `captured_pdf_attachment.jsonl` - prompt, PDF attachment, Read call, and result. Synthetic control PDF bytes are unchanged.
- `captured_text_attachment.jsonl` - prompt and text attachment with a line range. Synthetic specification text is unchanged.
- `captured_webfetch.jsonl` - prompt, WebFetch call, and summary. URLs, identities, instructions, and response text are replaced with example data; fetched byte count and parent relationships are preserved. Account metadata and timing are removed. Response bytes are not an exact capture.

Synthetic fixtures use invented identities and relationships absent from the captures:

- `synthetic_multirequest.jsonl` - an attachment serialized after a later request.
- `synthetic_delegation.jsonl` - a delegated request with its own attachment.
- `synthetic_null_attachment.jsonl` - an attachment without a body linked to a request.
