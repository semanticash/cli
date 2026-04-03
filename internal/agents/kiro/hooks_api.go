package kiro

import (
	"database/sql"
	"encoding/json"
)

type toolUsesPayload struct {
	Tools []toolEntry `json:"tools"`
}

type toolEntry struct {
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
	FileOp   string `json:"file_op"`
}

// BuildToolUsesJSON returns a serialized tool_uses payload for a file
// operation.
func BuildToolUsesJSON(filePath, fileOp string) sql.NullString {
	if filePath == "" {
		return sql.NullString{}
	}
	payload := toolUsesPayload{
		Tools: []toolEntry{
			{Name: ToolNameFileEdit, FilePath: filePath, FileOp: fileOp},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

// DecodeProjectPathFromSessionDir reverses the workspace directory encoding
// used under Kiro's session store.
func DecodeProjectPathFromSessionDir(dirName string) string {
	return decodeWorkspacePath(dirName)
}
