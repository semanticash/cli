package hooks

import (
	"sort"
	"sync"
)

// providerOrder defines the canonical display order. Names must
// match the provider Name() exactly; unknown providers fall to
// weight 100 and sort alphabetically among themselves.
var providerOrder = map[string]int{
	"claude-code": 0,
	"codex":       1,
	"cursor":      2,
	"copilot":     3,
	"gemini-cli":  4,
	"kiro-cli":    5,
	"kiro-ide":    6,
}

var (
	registryMu sync.RWMutex
	providers  = make(map[string]HookProvider)
)

// RegisterProvider registers a hook provider by name.
// Called by provider packages in their init() functions.
func RegisterProvider(p HookProvider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	providers[p.Name()] = p
}

// GetProvider returns the registered provider for the given name, or nil.
func GetProvider(name string) HookProvider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return providers[name]
}

// ListProviders returns all registered providers in canonical order.
func ListProviders() []HookProvider {
	registryMu.RLock()
	defer registryMu.RUnlock()
	result := make([]HookProvider, 0, len(providers))
	for _, p := range providers {
		result = append(result, p)
	}
	sortProviders(result)
	return result
}

// ListAvailableProviders returns registered providers whose agent
// is detected on the current machine, in canonical order.
func ListAvailableProviders() []HookProvider {
	all := ListProviders()
	var available []HookProvider
	for _, p := range all {
		if p.IsAvailable() {
			available = append(available, p)
		}
	}
	return available
}

func sortProviders(ps []HookProvider) {
	sort.Slice(ps, func(i, j int) bool {
		oi, oki := providerOrder[ps[i].Name()]
		oj, okj := providerOrder[ps[j].Name()]
		if !oki {
			oi = 100
		}
		if !okj {
			oj = 100
		}
		if oi != oj {
			return oi < oj
		}
		// Stable order among same-weight entries (the unknowns).
		return ps[i].Name() < ps[j].Name()
	})
}
