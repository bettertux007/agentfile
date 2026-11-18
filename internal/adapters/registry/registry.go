package registry

import (
	"fmt"
	"sort"

	"github.com/bettertux007/agentfile/internal/adapters"
	"github.com/bettertux007/agentfile/internal/adapters/claude"
	"github.com/bettertux007/agentfile/internal/adapters/cursor"
	"github.com/bettertux007/agentfile/internal/adapters/gemini"
	"github.com/bettertux007/agentfile/internal/adapters/opencode"
)

// Default returns the full Milestone 2 adapter registry (claude, opencode, gemini, cursor).
func Default() map[string]adapters.Adapter {
	list := []adapters.Adapter{
		claude.New(),
		opencode.New(),
		gemini.New(),
		cursor.New(),
	}
	out := make(map[string]adapters.Adapter, len(list))
	for _, a := range list {
		out[a.Name()] = a
	}
	return out
}

// Lookup returns the adapter for name or an error if unsupported.
func Lookup(reg map[string]adapters.Adapter, name string) (adapters.Adapter, error) {
	a, ok := reg[name]
	if !ok {
		return nil, fmt.Errorf("target adapter %q is not registered in this build", name)
	}
	return a, nil
}

// Names returns registered adapter names in sorted order.
func Names(reg map[string]adapters.Adapter) []string {
	out := make([]string, 0, len(reg))
	for k := range reg {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
