package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/service"
)

// toolDef is a simplified MCP tool definition.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// toolDefinitions returns the list of tools this MCP server exposes.
func toolDefinitions() []toolDef {
	return []toolDef{
		{
			Name:        "semantica_explain",
			Description: "Get a detailed explanation of a commit: AI attribution percentage, files changed, session info, and playbook summary if available.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ref": map[string]any{
						"type":        "string",
						"description": "Commit hash, prefix, or checkpoint ID to explain",
					},
				},
				"required": []string{"ref"},
			},
		},
	}
}

// callTool dispatches a tool call to the appropriate service.
func callTool(ctx context.Context, repoPath, name string, args json.RawMessage) (any, error) {
	switch name {
	case "semantica_explain":
		return callExplain(ctx, repoPath, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func callExplain(ctx context.Context, repoPath string, args json.RawMessage) (any, error) {
	var params struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	svc := service.NewExplainService()
	result, err := svc.Explain(ctx, service.ExplainInput{
		RepoPath: repoPath,
		Ref:      params.Ref,
	})
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(data)},
		},
	}, nil
}
