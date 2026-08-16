// Package failpoint provides explicitly injected, named test failpoints.
// Registries are ordinary values: there is no global hook that production
// code can accidentally enable.
package failpoint

import (
	"errors"
	"fmt"
	"sync"
)

var ErrUnknown = errors.New("failpoint: unknown failpoint")

// Registry stores failpoints and their hit counts. A new registry starts with
// every point disabled.
type Registry struct {
	mu     sync.RWMutex
	points map[string]*point
}

type point struct {
	enabled bool
	hits    uint64
}

func New() *Registry { return &Registry{points: make(map[string]*point)} }

// Register adds a disabled point. Registering a name twice is idempotent so
// component wiring can be assembled by multiple test helpers safely.
func (r *Registry) Register(name string) error {
	if name == "" {
		return errors.New("failpoint: name cannot be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.points == nil {
		r.points = make(map[string]*point)
	}
	if _, exists := r.points[name]; !exists {
		r.points[name] = &point{}
	}
	return nil
}

func (r *Registry) Enable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	point, ok := r.points[name]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknown, name)
	}
	point.enabled = true
	return nil
}

func (r *Registry) Disable(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	point, ok := r.points[name]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknown, name)
	}
	point.enabled = false
	return nil
}

// Enabled reports the current state of a registered point. Unknown points
// are disabled, which keeps accidental probes harmless.
func (r *Registry) Enabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	point, ok := r.points[name]
	return ok && point.enabled
}

// Hit records and reports an enabled point. Unknown names return ErrUnknown;
// registered but disabled points return (false, nil). Callers decide how to
// inject the failure (return an error, stop a worker, or pause a state
// machine); this package never panics or changes control flow on their behalf.
func (r *Registry) Hit(name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	point, ok := r.points[name]
	if !ok {
		return false, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	if !point.enabled {
		return false, nil
	}
	point.hits++
	return true, nil
}

func (r *Registry) Hits(name string) (uint64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if point, ok := r.points[name]; ok {
		return point.hits, nil
	}
	return 0, fmt.Errorf("%w %q", ErrUnknown, name)
}
