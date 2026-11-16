package backend

import (
	"context"
	"fmt"
	"sync"
)

// StaticConfig describes a driver over pre-existing SSH endpoints (lab machines, long-lived hosts).
//
// Nothing is created or destroyed: an endpoint is leased to one identity at a time and handed back on
// release. This driver exists so that keypod is useful without any orchestrator at all — and so that
// the core can be tested end to end without a cluster.
type StaticConfig struct {
	Name          string
	Endpoints     []string
	User          string
	PrivateKey    string
	HostKeyPolicy string
}

// State annotation keys understood by this driver.
const (
	AnnoState    = "state"    // "free" | "leased"
	AnnoIdentity = "identity" // identity currently holding the endpoint
)

// Static is an in-memory endpoint pool.
type Static struct {
	cfg StaticConfig

	mu    sync.Mutex
	owner map[string]string // endpoint -> identity
	next  int
}

// NewStatic validates the configuration and returns the driver.
func NewStatic(cfg StaticConfig) (*Static, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("static backend: name is required")
	}
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("static backend %q: at least one endpoint is required", cfg.Name)
	}
	if cfg.User == "" || cfg.PrivateKey == "" {
		return nil, fmt.Errorf("static backend %q: ssh user and private key are required", cfg.Name)
	}
	seen := map[string]bool{}
	for _, ep := range cfg.Endpoints {
		if ep == "" {
			return nil, fmt.Errorf("static backend %q: empty endpoint", cfg.Name)
		}
		if seen[ep] {
			return nil, fmt.Errorf("static backend %q: duplicate endpoint %q", cfg.Name, ep)
		}
		seen[ep] = true
	}
	return &Static{cfg: cfg, owner: make(map[string]string, len(cfg.Endpoints))}, nil
}

// Name implements Backend.
func (s *Static) Name() string { return s.cfg.Name }

// Acquire returns the endpoint already held by identity, or the next free one.
func (s *Static) Acquire(_ context.Context, identity string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An identity that already holds an endpoint keeps it: this is the whole point of the lease.
	for ep, id := range s.owner {
		if id == identity {
			return s.instance(ep, identity), nil
		}
	}

	n := len(s.cfg.Endpoints)
	for i := 0; i < n; i++ {
		idx := (s.next + i) % n
		ep := s.cfg.Endpoints[idx]
		if _, taken := s.owner[ep]; taken {
			continue
		}
		s.owner[ep] = identity
		s.next = (idx + 1) % n
		return s.instance(ep, identity), nil
	}
	return Instance{}, fmt.Errorf("static backend %q: %w (%d endpoints)", s.cfg.Name, ErrNoCapacity, n)
}

// Release frees the endpoint. Releasing an endpoint held by someone else is a no-op: ownership is only
// ever cleared by the identity that set it, so a stale release cannot evict a live lease.
func (s *Static) Release(_ context.Context, in Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.owner[in.ID]; ok && id == in.Annotations[AnnoIdentity] {
		delete(s.owner, in.ID)
	}
	return nil
}

// List reports every endpoint with its current state.
func (s *Static) List(_ context.Context) ([]Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.cfg.Endpoints))
	for _, ep := range s.cfg.Endpoints {
		out = append(out, s.instance(ep, s.owner[ep]))
	}
	return out, nil
}

// Adopt restores ownership recorded in annotations (used by the lease manager after a restart).
func (s *Static) Adopt(_ context.Context, ins []Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range ins {
		id := in.Annotations[AnnoIdentity]
		if id == "" || in.Annotations[AnnoState] != "leased" {
			continue
		}
		known := false
		for _, ep := range s.cfg.Endpoints {
			if ep == in.ID {
				known = true
				break
			}
		}
		if !known {
			continue
		}
		s.owner[in.ID] = id
	}
	return nil
}

func (s *Static) instance(endpoint, identity string) Instance {
	state := "free"
	if identity != "" {
		state = "leased"
	}
	return Instance{
		ID:            endpoint,
		Addr:          endpoint,
		User:          s.cfg.User,
		PrivateKey:    s.cfg.PrivateKey,
		HostKeyPolicy: s.cfg.HostKeyPolicy,
		Annotations: map[string]string{
			AnnoState:    state,
			AnnoIdentity: identity,
		},
	}
}
