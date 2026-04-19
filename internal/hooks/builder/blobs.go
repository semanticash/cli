package builder

import (
	"context"
	"encoding/json"

	"github.com/semanticash/cli/internal/agents/api"
)

// PutAndHash writes the payload to the blob store and returns its
// content hash. Returns an empty string when the blob store is nil
// or the Put call fails; the caller continues to assemble the event
// with the hash field left empty, matching the direct-emit
// silent-degradation contract.
func PutAndHash(ctx context.Context, bs api.BlobPutter, payload []byte) string {
	if bs == nil {
		return ""
	}
	h, _, err := bs.Put(ctx, payload)
	if err != nil {
		return ""
	}
	return h
}

// StorePromptPayload persists the raw bytes of a user or subagent
// prompt and returns the content hash. Thin wrapper over PutAndHash
// named for intent so call sites read naturally.
func StorePromptPayload(ctx context.Context, bs api.BlobPutter, prompt string) string {
	if prompt == "" {
		return ""
	}
	return PutAndHash(ctx, bs, []byte(prompt))
}

// SynthesizeAssistantBlob marshals the canonical assistant payload
// shape consumed by the attribution scorer and stores it in the
// blob store. The shape is:
//
//	{
//	  "type": "assistant",
//	  "message": {
//	    "content": [
//	      { "type": "tool_use", "name": <toolName>, "input": <inputJSON> }
//	    ]
//	  }
//	}
//
// inputJSON must already be normalized to the shape the scorer
// expects for the given tool. Providers whose hook payloads use
// different field names convert them to the canonical shape before
// calling this helper.
//
// Returns an empty string on marshal or blob-store failure.
func SynthesizeAssistantBlob(ctx context.Context, bs api.BlobPutter, toolName string, inputJSON json.RawMessage) string {
	if bs == nil {
		return ""
	}
	blob := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"name":  toolName,
					"input": inputJSON,
				},
			},
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return PutAndHash(ctx, bs, data)
}

// StoreWrappedHookProvenance stores the original hook payload under
// the shared envelope used by Claude, Copilot, Gemini, and Kiro CLI:
//
//	{ "tool_input": <toolInput>, "tool_response": <toolResponse> }
//
// The tool_response field is omitted when toolResponse is empty, so
// hook phases that do not carry a response (for example pre-tool-use)
// produce a smaller, wrapper-only blob.
//
// Cursor stores the raw payload without this wrapper and should not
// call into this helper; it keeps its own one-line helper per the
// matrix row 10 divergence.
//
// Returns an empty string on marshal or blob-store failure.
func StoreWrappedHookProvenance(ctx context.Context, bs api.BlobPutter, toolInput, toolResponse json.RawMessage) string {
	if bs == nil {
		return ""
	}
	blob := map[string]json.RawMessage{
		"tool_input": toolInput,
	}
	if len(toolResponse) > 0 {
		blob["tool_response"] = toolResponse
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return PutAndHash(ctx, bs, data)
}
