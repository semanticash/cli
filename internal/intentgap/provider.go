package intentgap

import (
	"errors"
	"fmt"

	"github.com/semanticash/cli/internal/llm"
)

// ErrNoInstalledProvider indicates that no supported local AI CLI is available.
var ErrNoInstalledProvider = errors.New("intentgap: no LLM CLI installed")

// InstalledProvider identifies the selected writer using API wire names.
type InstalledProvider struct {
	Name  string
	Model string
}

// writerNameToWireProvider maps local writer names to API provider values.
var writerNameToWireProvider = map[string]string{
	"claude_code": "claude_code",
	"codex":       "codex",
	"cursor":      "cursor_cli",
	"gemini_cli":  "gemini_cli",
	"copilot":     "copilot_cli",
	"kiro_cli":    "kiro_cli",
}

// MapWriterNameToWire returns the API provider value for a writer name.
func MapWriterNameToWire(writerName string) (string, bool) {
	wire, ok := writerNameToWireProvider[writerName]
	return wire, ok
}

// PickInstalledProvider returns the first installed, API-supported writer.
func PickInstalledProvider(reg *llm.WriterRegistry) (InstalledProvider, error) {
	if reg == nil {
		return InstalledProvider{}, ErrNoInstalledProvider
	}
	var firstUnknown string
	for _, w := range reg.List() {
		if w.Find() == "" {
			continue
		}
		wire, ok := MapWriterNameToWire(w.Name())
		if !ok {
			if firstUnknown == "" {
				firstUnknown = w.Name()
			}
			continue
		}
		return InstalledProvider{Name: wire, Model: w.Model()}, nil
	}
	if firstUnknown != "" {
		return InstalledProvider{}, fmt.Errorf("%w (installed writer %q has no wire mapping)", ErrNoInstalledProvider, firstUnknown)
	}
	return InstalledProvider{}, ErrNoInstalledProvider
}
