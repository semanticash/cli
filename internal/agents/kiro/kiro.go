// Package kiro contains shared identifiers and helpers used by the Kiro
// providers.
package kiro

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// ProviderNameIDE is the provider identifier for the Kiro IDE hook provider.
	ProviderNameIDE = "kiro-ide"

	// ProviderNameCLI is the provider identifier for the Kiro CLI hook provider.
	ProviderNameCLI = "kiro-cli"

	// ToolNameFileEdit marks Kiro actions that only support file-touch
	// attribution, such as renames with no old/new content delta.
	ToolNameFileEdit = "kiro_file_edit"

	// ToolNameWrite and ToolNameEdit are canonical tool names for events
	// that carry line-level payloads.
	ToolNameWrite = "Write"
	ToolNameEdit  = "Edit"
)

// WorkspaceKey derives a deterministic capture-state key from a workspace path
// and provider prefix.
func WorkspaceKey(prefix, absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return prefix + ":" + hex.EncodeToString(h[:8])
}
