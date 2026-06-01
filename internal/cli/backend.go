package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mwiget/kindbnkctl/internal/cluster"
)

// SelectedBackend reports which cluster backend this invocation drives,
// chosen from the binary's own name (argv[0] basename). Install the
// binary as `kindbnkctl` for kind (the default) and symlink
// `k3dbnkctl` → `kindbnkctl` to drive k3d from the same binary. Any
// name containing "k3d" selects k3d; everything else selects kind.
func SelectedBackend() cluster.Backend {
	return backendFromArg0(os.Args[0])
}

func backendFromArg0(arg0 string) cluster.Backend {
	base := strings.ToLower(filepath.Base(arg0))
	if strings.Contains(base, "k3d") {
		return cluster.BackendK3d
	}
	return cluster.BackendKind
}

// invocationName is the binary's own basename, used in help text and
// "next:" hints so a `k3dbnkctl` symlink reads consistently.
func invocationName() string {
	base := filepath.Base(os.Args[0])
	if base == "." || base == "/" || base == "" {
		return "kindbnkctl"
	}
	return base
}

// newProvisioner builds the Provisioner for the selected backend.
func newProvisioner(rt cluster.Runtime, out io.Writer) (cluster.Provisioner, error) {
	return cluster.NewProvisioner(SelectedBackend(), rt, out)
}
