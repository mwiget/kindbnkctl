package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/bnkforge"
	"github.com/mwiget/kindbnkctl/internal/cluster"
	"github.com/mwiget/kindbnkctl/internal/poc"
)

type destroyFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	keepNetworks   bool
}

func newDestroyCmd() *cobra.Command {
	f := &destroyFlags{}
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down the kind cluster (and bnk-forge registration), drop docker networks",
		Long: `Symmetric tear-down:

  1. bnk-forge unregister  (if bnk_forge.enabled and reachable)
  2. kind delete cluster
  3. docker network rm internal + external

Required gates:
  --yolo                   acknowledge destructive
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroy(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge destructive")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.keepNetworks, "keep-networks", false, "Don't remove the docker networks (useful when sharing the network with another cluster)")
	return cmd
}

func runDestroy(ctx context.Context, out io.Writer, f *destroyFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster,
		p.Metadata.Name, "destroy"); err != nil {
		return err
	}

	fmt.Fprintf(out, "PoC:     %s\nCluster: %s\n\n", p.Metadata.Name, p.Cluster.Name)

	rt, err := cluster.Detect(ctx, cluster.Runtime(p.Cluster.Provider))
	if err != nil {
		fmt.Fprintf(out, "WARN: container runtime detect failed: %v — will only attempt non-runtime steps\n", err)
	}
	kc := &cluster.Kind{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}
	dc := &cluster.DockerCLI{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}

	// 1. bnk-forge unregister.
	fmt.Fprintln(out, "[1/3] bnk-forge unregister ...")
	if !p.BNKForge.Enabled {
		fmt.Fprintln(out, "      skipped (bnk_forge.enabled=false)")
	} else {
		if err := unregisterFromBNKForge(ctx, out, p); err != nil {
			if errors.Is(err, bnkforge.ErrNotRunning) {
				fmt.Fprintf(out, "      bnk-forge not running — skipping. (%v)\n", err)
			} else {
				fmt.Fprintf(out, "      WARN: bnk-forge unregister failed: %v\n", err)
			}
		}
	}

	// 2. kind delete.
	fmt.Fprintln(out, "[2/3] kind delete cluster ...")
	if err := kc.EnsurePresent(); err != nil {
		fmt.Fprintf(out, "      WARN: %v\n", err)
	} else {
		if err := kc.DeleteCluster(ctx, p.Cluster.Name); err != nil {
			return err
		}
	}

	// 3. docker network rm. Best-effort cleanup for older PoCs that
	// had bnk-internal / bnk-external bridges; new PoCs leave the
	// Networks struct empty so this loop is a no-op.
	fmt.Fprintln(out, "[3/3] docker network rm ...")
	if f.keepNetworks {
		fmt.Fprintln(out, "      skipped (--keep-networks)")
	} else if rt != "" {
		for _, n := range []string{p.Networks.Internal.Name, p.Networks.External.Name} {
			if n == "" {
				continue
			}
			if err := dc.RemoveNetwork(ctx, n); err != nil {
				fmt.Fprintf(out, "      WARN: %v\n", err)
			}
		}
	}

	// Drop kubeconfig + e2e state so next run can't accidentally reuse it.
	_ = os.Remove(filepath.Join(repo, "artifacts", "kubeconfig"))
	_ = os.Remove(filepath.Join(repo, "artifacts", "e2e-state.json"))

	p.Status.Cluster = "destroyed"
	p.Status.Deploy = "destroyed"
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

func unregisterFromBNKForge(ctx context.Context, out io.Writer, p *poc.PoC) error {
	cfg := bnkforge.Config{
		RepoPath:      p.BNKForge.RepoPath,
		URL:           p.BNKForge.URL,
		AdminUsername: p.BNKForge.AdminUsername,
		AdminPassword: p.BNKForge.AdminPassword,
	}.WithDefaults()
	if err := bnkforge.RequireRunning(ctx, cfg, out); err != nil {
		return err
	}
	cli := bnkforge.NewClient(cfg)
	if err := cli.Login(ctx, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return err
	}
	projectID, found, err := cli.FindProjectByName(ctx, p.Metadata.Name)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "      no project %q in bnk-forge — nothing to unregister\n", p.Metadata.Name)
		return nil
	}
	clusters, err := cli.ListProjectClusters(ctx, projectID)
	if err == nil {
		for _, c := range clusters {
			if c.Name == p.Metadata.Name {
				if err := cli.DeleteCluster(ctx, c.ID); err != nil {
					fmt.Fprintf(out, "      WARN: delete cluster %d: %v\n", c.ID, err)
				} else {
					fmt.Fprintf(out, "      deleted cluster registration (id=%d)\n", c.ID)
				}
				break
			}
		}
	}
	if err := cli.DeleteProject(ctx, projectID); err != nil {
		return err
	}
	fmt.Fprintf(out, "      deleted project %q (id=%d)\n", p.Metadata.Name, projectID)
	return nil
}
