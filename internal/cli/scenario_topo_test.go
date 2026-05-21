package cli

import (
	"context"
	"testing"

	"github.com/mwiget/kindbnkctl/internal/scenarios"
)

type fakeScenario struct {
	name string
	deps []string
}

func (f fakeScenario) Name() string                                          { return f.name }
func (f fakeScenario) Title() string                                         { return f.name }
func (f fakeScenario) Rating() scenarios.Rating                              { return scenarios.Green }
func (f fakeScenario) Description() string                                   { return "" }
func (f fakeScenario) Dependencies() []string                                { return f.deps }
func (f fakeScenario) Manifests(*scenarios.Context) ([]string, error)        { return nil, nil }
func (f fakeScenario) Apply(*scenarios.Context) error                        { return nil }
func (f fakeScenario) Verify(*scenarios.Context) scenarios.Result            { return scenarios.Result{} }
func (f fakeScenario) Cleanup(*scenarios.Context) error                      { return nil }
func (f fakeScenario) Run(ctx context.Context) (scenarios.Result, error)     { return scenarios.Result{}, nil }

func TestTopoSortByDeps_DependencyBeforeDependent(t *testing.T) {
	// Alphabetical order would put ai-* before bgp-peer-frr, breaking deps.
	in := []scenarios.Scenario{
		fakeScenario{name: "ai-semantic-cache", deps: []string{"bgp-peer-frr"}},
		fakeScenario{name: "ai-token-counting", deps: []string{"bgp-peer-frr"}},
		fakeScenario{name: "bgp-peer-frr"},
		fakeScenario{name: "cluster-wide-watch"},
		fakeScenario{name: "external-resource-pool", deps: []string{"bgp-peer-frr"}},
	}
	got, err := topoSortByDeps(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	pos := map[string]int{}
	for i, s := range got {
		pos[s.Name()] = i
	}
	for _, s := range in {
		for _, dep := range s.Dependencies() {
			if pos[dep] >= pos[s.Name()] {
				t.Errorf("dep %q must come before %q (got pos %d vs %d)", dep, s.Name(), pos[dep], pos[s.Name()])
			}
		}
	}
	if len(got) != len(in) {
		t.Errorf("got %d scenarios, want %d", len(got), len(in))
	}
}

func TestTopoSortByDeps_IgnoresDepsOutsideSet(t *testing.T) {
	// proxy-protocol-l4 depends on bgp-peer-frr but is amber-filtered out.
	// Topo sort should silently ignore dangling deps.
	in := []scenarios.Scenario{
		fakeScenario{name: "ai-semantic-cache", deps: []string{"bgp-peer-frr", "nonexistent"}},
		fakeScenario{name: "bgp-peer-frr"},
	}
	got, err := topoSortByDeps(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got[0].Name() != "bgp-peer-frr" || got[1].Name() != "ai-semantic-cache" {
		t.Errorf("got order [%s, %s], want [bgp-peer-frr, ai-semantic-cache]", got[0].Name(), got[1].Name())
	}
}

func TestTopoSortByDeps_DetectsCycle(t *testing.T) {
	in := []scenarios.Scenario{
		fakeScenario{name: "a", deps: []string{"b"}},
		fakeScenario{name: "b", deps: []string{"a"}},
	}
	if _, err := topoSortByDeps(in); err == nil {
		t.Errorf("expected cycle error, got nil")
	}
}
