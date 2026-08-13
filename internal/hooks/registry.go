package hooks

import "sort"

// providerOrder defines the canonical display order. Unknown providers sort
// alphabetically after the listed providers.
var providerOrder = map[string]int{
	"claude-code": 0,
	"codex":       1,
	"cursor":      2,
	"copilot":     3,
	"gemini-cli":  4,
	"kiro-cli":    5,
	"kiro-ide":    6,
}

// Registry stores hook providers. It is safe for concurrent reads after
// construction.
type Registry struct {
	providers map[string]HookProvider
}

// NewRegistry constructs a registry. List returns providers in canonical order.
func NewRegistry(providers ...HookProvider) *Registry {
	r := &Registry{providers: make(map[string]HookProvider, len(providers))}
	for _, p := range providers {
		r.providers[p.Name()] = p
	}
	return r
}

// Get returns the named provider, or nil when it is not registered.
func (r *Registry) Get(name string) HookProvider {
	if r == nil {
		return nil
	}
	return r.providers[name]
}

// List returns the registered providers in canonical order.
func (r *Registry) List() []HookProvider {
	if r == nil {
		return nil
	}
	out := make([]HookProvider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sortProviders(out)
	return out
}

// ListAvailable returns providers available on this host in canonical order.
func (r *Registry) ListAvailable() []HookProvider {
	if r == nil {
		return nil
	}
	all := r.List()
	out := make([]HookProvider, 0, len(all))
	for _, p := range all {
		if p.IsAvailable() {
			out = append(out, p)
		}
	}
	return out
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
