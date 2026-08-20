# Copilot CLI events fixture and schema

Anonymized reference for GitHub Copilot CLI's session transcript and hook payloads. Derived from a real Copilot
CLI session captured on macOS against a demo repo, 2026-08-16 (Copilot CLI 1.0.80). All ids, paths, tokens, and
prompt/response text in `events-sample.jsonl` are placeholders; the structure and key names are verbatim.

## Where it lives at runtime

`~/.copilot/session-state/<session-uuid>/events.jsonl` — one JSON object per line, top-level keys
`{id, parentId, timestamp, type, data}`. `agentStop.input.transcriptPath` points at this same file.

## Final response

Copilot delivers no final-response text on a hook. `agentStop` provides `transcriptPath`, which identifies the
session transcript. The final visible response is the **later assistant message in the turn**: the
`assistant.message` whose `data.toolRequests` is empty. Both assistant messages share `turnId:"0"`; the earlier
one carries the tool request and the later one carries the answer. Semantica does not currently store a Copilot
final response. It has no response hook or transcript fallback, so the manifest records
`response_status=unsupported` for Copilot turns.

## Tool pre/post pairing

`preToolUse` carries `toolCalls[].id`, but `postToolUse` omits it (it has only `toolName` + `toolArgs`). Copilot
therefore provides **no id shared by the pre/post pair**; the only possible correlation is a synthetic
`(sessionId + toolName + toolArgs)` match, which collides on duplicate identical commands. Copilot shell
tool-delta attribution is unsupported for this reason. `agentStop` also carries `stop_hook_active` and a
`stopReason`, so a blocked/continued completion is detectable in the payload.

## Copilot hook payloads (`hook.start.data.input`)

| hookType | input keys | shared pre/post id? |
|---|---|---|
| `userPromptSubmitted` | `prompt, sessionId, cwd, timestamp` | — |
| `sessionStart` | `source, initialPrompt, sessionId, cwd, timestamp` | — |
| `preToolUse` | `sessionId, cwd, toolCalls:[{id, name, args}]` | has `id` |
| `postToolUse` | `sessionId, cwd, toolName, toolArgs, toolResult{...}` | **no id** |
| `agentStop` | `transcriptPath, stopReason, stop_hook_active, sessionId, cwd` | — |
| `sessionEnd` | `reason, sessionId, cwd` | — |
