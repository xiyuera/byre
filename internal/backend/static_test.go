package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newStatic(t *testing.T, endpoints ...string) *Static {
	t.Helper()
	s, err := NewStatic(StaticConfig{
		Name:          "lab",
		Endpoints:     endpoints,
		User:          "sandbox",
		PrivateKey:    "/keys/pool",
		HostKeyPolicy: "strict",
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return s
}

func TestNewStaticValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  StaticConfig
		want string
	}{
		{"no name", StaticConfig{Endpoints: []string{"a:22"}, User: "u", PrivateKey: "/k"}, "name is required"},
		{"no endpoints", StaticConfig{Name: "x", User: "u", PrivateKey: "/k"}, "at least one endpoint"},
		{"no user", StaticConfig{Name: "x", Endpoints: []string{"a:22"}, PrivateKey: "/k"}, "user and private key"},
		{"duplicate", StaticConfig{Name: "x", Endpoints: []string{"a:22", "a:22"}, User: "u", PrivateKey: "/k"}, "duplicate endpoint"},
		{"empty endpoint", StaticConfig{Name: "x", Endpoints: []string{""}, User: "u", PrivateKey: "/k"}, "empty endpoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStatic(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStaticSameIdentityKeepsEndpoint(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, "192.0.2.11:22", "192.0.2.12:22")

	first, err := s.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := s.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire again: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("same identity got different endpoints: %q then %q", first.ID, second.ID)
	}
	if first.Annotations[AnnoState] != "leased" || first.Annotations[AnnoIdentity] != "identity-a" {
		t.Fatalf("unexpected annotations: %v", first.Annotations)
	}
}

func TestStaticTwoIdentitiesDiffer(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, "192.0.2.11:22", "192.0.2.12:22")

	a, err := s.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	b, err := s.Acquire(ctx, "identity-b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two identities share endpoint %q", a.ID)
	}
}

func TestStaticExhaustion(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, "192.0.2.11:22")

	if _, err := s.Acquire(ctx, "identity-a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := s.Acquire(ctx, "identity-b")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
}

func TestStaticReleaseIsIdempotentAndScoped(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, "192.0.2.11:22")

	a, err := s.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A release carrying someone else's identity must not free the endpoint.
	wrong := a
	wrong.Annotations = map[string]string{AnnoState: "leased", AnnoIdentity: "identity-b"}
	if err := s.Release(ctx, wrong); err != nil {
		t.Fatalf("release (wrong owner): %v", err)
	}
	listed, _ := s.List(ctx)
	if listed[0].Annotations[AnnoState] != "leased" {
		t.Fatalf("wrong-owner release freed the endpoint")
	}

	// The owner can release, twice.
	for i := 0; i < 2; i++ {
		if err := s.Release(ctx, a); err != nil {
			t.Fatalf("release #%d: %v", i, err)
		}
	}
	listed, _ = s.List(ctx)
	if listed[0].Annotations[AnnoState] != "free" {
		t.Fatalf("endpoint not freed: %v", listed[0].Annotations)
	}

	// After release another identity may take it.
	if _, err := s.Acquire(ctx, "identity-b"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestStaticAdoptRestoresOwner(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, "192.0.2.11:22")

	if err := s.Adopt(ctx, []Instance{{
		ID:   "192.0.2.11:22",
		Addr: "192.0.2.11:22",
		Annotations: map[string]string{
			AnnoState:    "leased",
			AnnoIdentity: "identity-a",
		},
	}}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	got, err := s.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire after adopt: %v", err)
	}
	if got.ID != "192.0.2.11:22" {
		t.Fatalf("adopted endpoint not returned: %q", got.ID)
	}
	if _, err := s.Acquire(ctx, "identity-b"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("adopted endpoint was also handed to another identity: %v", err)
	}
}
