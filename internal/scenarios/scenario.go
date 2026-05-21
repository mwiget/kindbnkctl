// Package scenarios is the test-case framework for the kindbnkctl
// scenarios subcommand. Each scenario maps to one F5 how-to from
// clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ and
// exercises a slice of BNK functionality end-to-end against the
// running cluster — applying manifests, asserting reconciled state,
// and cleaning up.
//
// Rating is a stable hint to operators (and `scenario list`) about
// whether the scenario can actually run in the kindbnkctl 2-node /
// demo-mode TMM shape:
//
//	Green  — fully testable here
//	Amber  — partially testable; some assertions are skipped or
//	         relaxed because the kind shape can't reproduce them
//	         (e.g. forcing a TMM panic for core-file collection)
//	Red    — not testable; requires real DPUs, BGP peers, etc.
//	         Listed for discoverability, never executed.
package scenarios

import (
	"context"
	"io"

	"github.com/mwiget/kindbnkctl/internal/deploy"
	"github.com/mwiget/kindbnkctl/internal/poc"
)

// Rating qualifies how much of the underlying F5 how-to we can
// actually exercise in the kindbnkctl 2-node + demo-TMM cluster.
type Rating string

const (
	Green Rating = "green"
	Amber Rating = "amber"
	Red   Rating = "red"
)

// Assertion is one check inside Verify. Rich enough that the report
// reader can see exactly what passed / failed without running the
// scenario again. Want: short human-readable Description, the actual
// value observed (Got, optional), the OK flag. Failed assertions
// don't short-circuit Verify by themselves — the scenario decides
// when to bail.
type Assertion struct {
	Description string `json:"description"`
	OK          bool   `json:"ok"`
	Got         string `json:"got,omitempty"`
}

// Result is what each scenario returns from Apply+Verify. Status mirrors
// the e2e phase report vocabulary so the rollup CLI can use one renderer.
type Result struct {
	Status     string      `json:"status"` // ok | failed | skipped | dry-run
	Summary    string      `json:"summary"`
	Details    string      `json:"details,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Manifest   string      `json:"manifest_path,omitempty"`
}

// AllPassed returns true when every assertion in r is OK. Empty
// assertion list returns true — Summary alone suffices for trivial
// scenarios.
func (r Result) AllPassed() bool {
	for _, a := range r.Assertions {
		if !a.OK {
			return false
		}
	}
	return true
}

// Context is the small bundle every scenario needs at runtime.
type Context struct {
	Ctx context.Context
	PoC *poc.PoC
	// PoCDir is the absolute path of the operator's PoC repo. Used as
	// the parent of artifacts/scenarios/<name>/ where each scenario
	// writes rendered manifests + per-run logs.
	PoCDir string
	// Runner wraps kubectl/helm with the localized kubeconfig already
	// pre-configured. Scenarios use Runner.Apply for manifest application
	// and Runner.Kubectl / KubectlCapture for assertions.
	Runner *deploy.Runner
	// Out is where progress lines are streamed.
	Out io.Writer
	// DryRun: render manifests but apply nothing.
	DryRun bool
	// Verbose: surface per-assertion lines + Result.Details to Out.
	// JSON report always carries them regardless.
	Verbose bool
}

// Scenario is the interface every test case implements. Methods are
// invoked in this order: Manifests() once for artifact persistence,
// then Apply, then Verify; Cleanup is invoked by `scenario clean`.
type Scenario interface {
	// Name is the kebab-case identifier used on the CLI.
	Name() string
	// Title is the human-readable F5 how-to title this maps to.
	Title() string
	// Rating tells `scenario list` whether to surface it as runnable.
	Rating() Rating
	// Description is one paragraph explaining what's tested + what isn't.
	Description() string
	// Dependencies lists other scenario names this one logically
	// relies on (e.g. "bgp-peer-frr" if we expect BGP to already
	// work). `scenario run --all` topo-sorts by these so deps run
	// before their dependents. A single-name `scenario run` does
	// NOT auto-chain — verify just surfaces "dep not running" as
	// an assertion so the operator decides whether to start it.
	Dependencies() []string

	// Manifests renders all manifest files into <PoCDir>/artifacts/
	// scenarios/<Name>/ and returns the on-disk paths. Pure render —
	// no kube I/O. Always safe to call (dry-run or not).
	Manifests(*Context) ([]string, error)

	// Apply pushes the rendered manifests into the cluster. Called
	// AFTER Manifests; Apply must read what Manifests just wrote so
	// the operator can inspect the exact bytes that were applied.
	Apply(*Context) error

	// Verify asserts the expected post-Apply state. Idempotent and
	// repeatable: callers may invoke it again after a wait.
	Verify(*Context) Result

	// Cleanup undoes Apply. Idempotent — a missing namespace / object
	// is not an error.
	Cleanup(*Context) error
}

// Registry holds every scenario this binary knows about. Scenarios
// register themselves at init() time via Register(s).
var registry []Scenario

// Register adds s to the global scenario list. Safe to call from
// package init functions. Duplicate Name() panics — that's a build-
// time programmer bug, not something an operator can trigger.
func Register(s Scenario) {
	for _, existing := range registry {
		if existing.Name() == s.Name() {
			panic("scenarios: duplicate registration: " + s.Name())
		}
	}
	registry = append(registry, s)
}

// All returns the registered scenarios in registration order.
func All() []Scenario { return append([]Scenario(nil), registry...) }

// Find returns the scenario with the given name, or nil.
func Find(name string) Scenario {
	for _, s := range registry {
		if s.Name() == name {
			return s
		}
	}
	return nil
}
