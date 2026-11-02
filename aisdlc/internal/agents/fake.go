package agents

import (
	"context"
	"fmt"
	"sync"
)

// Fake is a scriptable adapter for tests. Handler receives a 1-based call number.
type Fake struct {
	Handler func(n int, req RunRequest) (RunResult, error)
	Calls   []RunRequest

	mu sync.Mutex
}

func (f *Fake) Name() string { return "fake" }
func (f *Fake) Capabilities() Capabilities {
	return Capabilities{ReadOnlySandbox: true}
}

func (f *Fake) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, req)
	n := len(f.Calls)
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return RunResult{ExitCode: 1}, fmt.Errorf("fake adapter: no handler set")
	}
	return h(n, req)
}
