package agents

import (
	"fmt"
	"sort"
	"strings"
)

// Registry maps adapter names ("claude", "agy", "llmapi", "demo", ...) to implementations.
type Registry struct {
	m map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{m: map[string]Adapter{}} }

func (r *Registry) Register(a Adapter) { r.m[a.Name()] = a }

func (r *Registry) Get(name string) (Adapter, error) {
	if a, ok := r.m[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown adapter %q (known: %s)", name, strings.Join(r.Names(), ", "))
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
