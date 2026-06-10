package intentgap

import (
	"errors"
	"fmt"

	"github.com/semanticash/cli/internal/llm"
)

// ErrNoInstalledProvider is returned when no writer in the registry
// has a locatable binary on the host. The intent-gap upload path
// treats this as a skip reason rather than an error: there is nothing
// useful to record for a machine with no LLM CLI installed.
var ErrNoInstalledProvider = errors.New("intentgap: no LLM CLI installed")

// InstalledProvider is the choice of writer the upload path records
// in the provider/model fields. The Name has already been mapped to
// the canonical wire form the server's upload validator accepts;
// callers do not need to re-translate.
type InstalledProvider struct {
	Name  string
	Model string
}

// writerNameToWireProvider translates the locally stable writer name
// (used in attribution records and logs) to the canonical wire enum
// the intent-gap upload endpoint accepts. Writer names diverge from
// the wire enum for historical reasons (e.g. "cursor" the writer vs
// "cursor_cli" the API enum); this mapper centralizes the translation
// so a renamed writer is a single-line fix.
var writerNameToWireProvider = map[string]string{
	"claude_code": "claude_code",
	"codex":       "codex",
	"cursor":      "cursor_cli",
	"gemini_cli":  "gemini_cli",
	"copilot":     "copilot_cli",
	"kiro_cli":    "kiro_cli",
}

// MapWriterNameToWire returns the canonical wire provider name for a
// given writer Name(), or ("", false) when the writer is unknown.
// Exposed so tests and other call sites can reuse the same mapping.
func MapWriterNameToWire(writerName string) (string, bool) {
	wire, ok := writerNameToWireProvider[writerName]
	return wire, ok
}

// PickInstalledProvider walks the registry in fallback order and
// returns the first writer whose Find() resolves to a binary, mapped
// to the wire enum. Writers with an unknown name are skipped (rather
// than uploaded under a guessed enum), which means a freshly added
// writer requires both a new map entry above and a matching enum on
// the server before it can drive uploads.
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
