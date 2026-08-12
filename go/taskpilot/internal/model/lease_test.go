package model_test

import (
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/canonical"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
)

func TestAsLeaseRequiresExactEnvelopeShape(t *testing.T) {
	valid := model.LeaseRef(model.Lease{Kind: "hyperv", ID: "vm-a", State: "base"})
	if lease, ok := model.AsLease(valid); !ok || lease.ID != "vm-a" {
		t.Fatalf("AsLease = %#v, %v", lease, ok)
	}
	cases := []any{
		map[string]any{"$lease": map[string]any{"kind": "hyperv"}},
		map[string]any{"$lease": map[string]any{"kind": "hyperv", "id": "vm", "state": "base", "extra": true}},
		map[string]any{"$lease": map[string]any{"kind": "hyperv", "id": "vm", "state": "base"}, "other": true},
	}
	for _, value := range cases {
		if _, ok := model.AsLease(value); ok {
			t.Fatalf("accepted malformed lease: %#v", value)
		}
	}
}

func TestLeaseIdentityDropsPhysicalIDAndKeepsState(t *testing.T) {
	hash := func(lease model.Lease) string {
		t.Helper()
		projected := model.ProjectLeaseIdentityMap(map[string]any{"runsOn": model.LeaseRef(lease)})
		value, err := canonical.Hash("test", projected)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	a := hash(model.Lease{Kind: "hyperv", ID: "vm-a", State: "s1"})
	b := hash(model.Lease{Kind: "hyperv", ID: "vm-b", State: "s1"})
	if a != b {
		t.Fatal("physical lease ID affected node identity")
	}
	c := hash(model.Lease{Kind: "hyperv", ID: "vm-a", State: "s2"})
	if a == c {
		t.Fatal("lease state did not affect node identity")
	}
}

func TestLeaseIdentityProjectionRecurses(t *testing.T) {
	input := map[string]any{
		"nested": []any{map[string]any{
			"lease": model.LeaseRef(model.Lease{Kind: "hyperv", ID: "vm-a", State: "base"}),
		}},
	}
	out := model.ProjectLeaseIdentityMap(input)
	lease := out["nested"].([]any)[0].(map[string]any)["lease"].(map[string]any)["$lease"].(map[string]any)
	if _, ok := lease["id"]; ok {
		t.Fatal("nested physical ID survived identity projection")
	}
}
