package backend

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommandAcquireParsesTheContract(t *testing.T) {
	c, err := NewCommand(CommandConfig{
		Name:          "cloud",
		Acquire:       []string{"sh", "-c", `printf '{"id":"vm-1","addr":"192.0.2.9:22"}'`},
		Timeout:       5 * time.Second,
		User:          "ci",
		PrivateKey:    "/etc/keys/ci",
		HostKeyPolicy: "accept-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.Acquire(context.Background(), "fp")
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != "vm-1" || in.Addr != "192.0.2.9:22" {
		t.Fatalf("parsed %+v", in)
	}
	// user/key omitted by the program fall back to the backend's ssh block
	if in.User != "ci" || in.PrivateKey != "/etc/keys/ci" {
		t.Fatalf("defaults not applied: %+v", in)
	}
}

func TestCommandAcquireRejectsNonJSON(t *testing.T) {
	c, _ := NewCommand(CommandConfig{
		Name: "bad", Acquire: []string{"sh", "-c", "echo not-json"},
		Timeout: 5 * time.Second, User: "u", PrivateKey: "k",
	})
	if _, err := c.Acquire(context.Background(), "fp"); err == nil ||
		!strings.Contains(err.Error(), "not one JSON object") {
		t.Fatalf("want a parse error naming the contract, got %v", err)
	}
}

func TestCommandAcquireFailureIsNotSilent(t *testing.T) {
	c, _ := NewCommand(CommandConfig{
		Name: "fails", Acquire: []string{"sh", "-c", "echo boom >&2; exit 3"},
		Timeout: 5 * time.Second, User: "u", PrivateKey: "k",
	})
	_, err := c.Acquire(context.Background(), "fp")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr must reach the caller, got %v", err)
	}
}

func TestCommandListWithoutListerIsNotAnError(t *testing.T) {
	c, _ := NewCommand(CommandConfig{
		Name: "nolist", Acquire: []string{"true"}, Timeout: time.Second, User: "u", PrivateKey: "k",
	})
	ins, err := c.List(context.Background())
	if err != nil || ins != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", ins, err)
	}
}

func TestCommandListReturnsInstances(t *testing.T) {
	c, _ := NewCommand(CommandConfig{
		Name: "listed", Acquire: []string{"true"},
		List:    []string{"sh", "-c", `printf '[{"id":"a","addr":"1.2.3.4:22"},{"id":"b","addr":"5.6.7.8:22"}]'`},
		Timeout: 5 * time.Second, User: "u", PrivateKey: "k",
	})
	ins, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 2 || ins[1].ID != "b" {
		t.Fatalf("got %+v", ins)
	}
}

func TestCommandAcquireCarriesAnnotations(t *testing.T) {
	c, err := NewCommand(CommandConfig{
		Name: "cloud",
		Acquire: []string{"sh", "-c",
			`printf '{"id":"vm-1","addr":"192.0.2.9:22","annotations":{"byre.io/identity":"fp-1"}}'`},
		Timeout: 5 * time.Second, User: "ci", PrivateKey: "/etc/keys/ci",
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.Acquire(context.Background(), "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Annotations[LabelIdentity]; got != "fp-1" {
		t.Fatalf("annotations were dropped: %+v", in.Annotations)
	}
}

// TestCommandListCarriesAnnotations is the other half of the blocker: a lister that reports
// byre.io/identity must reach the manager with that annotation, or adoption can never recognise it.
func TestCommandListCarriesAnnotations(t *testing.T) {
	c, err := NewCommand(CommandConfig{
		Name: "cloud", Acquire: []string{"true"},
		List: []string{"sh", "-c",
			`printf '[{"id":"a","addr":"1.2.3.4:22","annotations":{"byre.io/identity":"fp-1"}},{"id":"b","addr":"5.6.7.8:22"}]'`},
		Timeout: 5 * time.Second, User: "u", PrivateKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	ins, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 2 {
		t.Fatalf("got %d instances", len(ins))
	}
	if got := ins[0].Annotations[LabelIdentity]; got != "fp-1" {
		t.Fatalf("list annotations were dropped: %+v", ins[0].Annotations)
	}
	if ins[1].Annotations != nil {
		t.Fatalf("an instance with no annotations must not gain an empty map: %+v", ins[1].Annotations)
	}
}
