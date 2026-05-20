package cli

import (
	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/version"
)

// NewRootCmd assembles the cobra command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kindbnkctl",
		Short: "Deploy F5 BIG-IP Next for Kubernetes (BNK) on a kind cluster with demo-mode TMM",
		Long: `kindbnkctl provisions a BNK ` + version.BNKVersion + ` deployment on a
two-node kind cluster (1 combined control-plane + worker, 1 worker
dedicated to TMM in demo mode):

  cluster up  -> create the kind cluster, install Calico, attach
                 internal + external docker networks, label the TMM
                 worker, fetch kubeconfig
  deploy      -> install BNK platform (cert-manager, FLO, License CR,
                 CNEInstance with demoMode=true)

Each PoC lives in its own local dir (see "init"). poc.yaml holds the
full declarative state needed to tear down and redeploy.

Run "kindbnkctl doctor" after install to verify docker/kind/kubectl/helm
are reachable.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newInitCmd(),
		newValidateCmd(),
		newDoctorCmd(),
		newClusterCmd(),
		newDeployCmd(),
		newDestroyCmd(),
		newE2ECmd(),
		newScenarioCmd(),
		newBNKForgeCmd(),
		newVersionCmd(),
	)
	return root
}
