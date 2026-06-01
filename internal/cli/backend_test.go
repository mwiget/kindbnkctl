package cli

import (
	"testing"

	"github.com/mwiget/kindbnkctl/internal/cluster"
)

func TestBackendFromArg0(t *testing.T) {
	cases := []struct {
		arg0 string
		want cluster.Backend
	}{
		{"/usr/local/bin/kindbnkctl", cluster.BackendKind},
		{"kindbnkctl", cluster.BackendKind},
		{"./bin/kindbnkctl", cluster.BackendKind},
		{"/usr/local/bin/k3dbnkctl", cluster.BackendK3d},
		{"k3dbnkctl", cluster.BackendK3d},
		{"K3Dbnkctl", cluster.BackendK3d}, // case-insensitive
		{"somethingelse", cluster.BackendKind},
	}
	for _, c := range cases {
		if got := backendFromArg0(c.arg0); got != c.want {
			t.Errorf("backendFromArg0(%q) = %q, want %q", c.arg0, got, c.want)
		}
	}
}
