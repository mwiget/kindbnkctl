package cluster

import (
	"slices"
	"testing"
)

func TestKindEnvDocker(t *testing.T) {
	env := RuntimeDocker.KindEnv()
	if len(env) != 0 {
		t.Errorf("docker should need no extra env, got %v", env)
	}
}

func TestKindEnvPodman(t *testing.T) {
	env := RuntimePodman.KindEnv()
	if !slices.Contains(env, "KIND_EXPERIMENTAL_PROVIDER=podman") {
		t.Errorf("podman env missing KIND_EXPERIMENTAL_PROVIDER, got %v", env)
	}
}

func TestKindEnvEmpty(t *testing.T) {
	// Empty Runtime is what callers see when Detect fails; KindEnv
	// must not panic and must return no entries.
	env := Runtime("").KindEnv()
	if len(env) != 0 {
		t.Errorf("empty runtime should yield no env, got %v", env)
	}
}
