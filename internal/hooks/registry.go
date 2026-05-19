package hooks

import "sort"

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

// Registry is the explicit-injection container for hook providers.
// Production wiring lives in internal/providers/composition.go,
// which builds a Registry over the full canonical set via
// NewHookRegistry. Tests construct their own Registry inline with
// NewRegistry over just the providers they need. There is no
// package-level default registry; callers always pass an explicit
// instance.
//
// Get and List are safe for concurrent reads after construction;
// the constructor copies its arguments into an internal map so
// callers can mutate the input slice without affecting the
// registry.
type Registry struct {
	providers map[string]HookProvider
}

// NewRegistry constructs a Registry over the given hook providers.
// Order of List() output is canonical (see providerOrder), not the
// argument order, so anchors that want deterministic iteration get
// the same order every consumer sees.
func NewRegistry(providers ...HookProvider) *Registry {
	r := &Registry{providers: make(map[string]HookProvider, len(providers))}
	for _, p := range providers {
		r.providers[p.Name()] = p
	}
	return r
}

// Get returns the registered provider for the given name, or nil
// when nothing is registered under that name. Callers must handle
// the nil case (a hook payload may report a provider this binary
// wasn't built with, or a future provider that's unknown today).
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

// ListAvailable returns the subset of registered providers whose
// IsAvailable() reports true on the current host, in canonical
// order. Used by health checks and `semantica agents` to filter
// the full set to what's actually installed.
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
