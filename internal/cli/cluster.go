package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/bnkforge"
	"github.com/mwiget/kindbnkctl/internal/cluster"
	"github.com/mwiget/kindbnkctl/internal/deploy"
	"github.com/mwiget/kindbnkctl/internal/embedded"
	"github.com/mwiget/kindbnkctl/internal/poc"
	"github.com/mwiget/kindbnkctl/internal/version"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Bring up (or down) the kind cluster",
	}
	cmd.AddCommand(newClusterUpCmd())
	return cmd
}

type clusterUpFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	skipBNKForge   bool
}

func newClusterUpCmd() *cobra.Command {
	f := &clusterUpFlags{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create the kind cluster, install Calico, attach docker networks, label TMM worker (DESTRUCTIVE)",
		Long: `Drive the kind cluster bring-up:

  1. Container-runtime preflight
  2. Render kind.yaml + ensure cluster exists (kind create cluster)
  3. Apply Calico manifest (default CNI is disabled in our kind.yaml)
  4. Create internal + external docker bridge networks and attach both
     to every node container (control-plane and worker)
  5. Label the worker node app=f5-tmm for TMM nodeSelector
  6. Fetch kubeconfig to artifacts/kubeconfig (mode 0600)
  7. If bnk_forge.enabled and the local stack is reachable, register
     the cluster with bnk-forge. Soft-skip on absence.

Required gates:
  --yolo                   acknowledge the cluster is recreated/written
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterUp(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster creation is destructive")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.skipBNKForge, "skip-bnk-forge", false, "Skip bnk-forge auto-registration even if enabled")
	return cmd
}

func runClusterUp(ctx context.Context, out io.Writer, f *clusterUpFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster,
		p.Metadata.Name, "cluster bring-up"); err != nil {
		return err
	}
	if r := p.Validate(); !r.Valid() {
		for _, e := range r.Errors {
			fmt.Fprintln(out, "  ✗", e)
		}
		return fmt.Errorf("poc.yaml is invalid — fix above and re-run `kindbnkctl validate`")
	}

	fmt.Fprintf(out, "PoC:        %s  (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
	fmt.Fprintf(out, "Cluster:    %s  (provider=%s)\n", p.Cluster.Name, p.Cluster.Provider)
	fmt.Fprintf(out, "Networks:   %s (%s), %s (%s)\n\n",
		p.Networks.Internal.Name, p.Networks.Internal.Subnet,
		p.Networks.External.Name, p.Networks.External.Subnet)

	// 1. Container runtime.
	fmt.Fprintln(out, "[1/7] Container-runtime preflight ...")
	rt, err := cluster.Detect(ctx, cluster.Runtime(p.Cluster.Provider))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      using %s\n", rt)

	kc := &cluster.Kind{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}
	if err := kc.EnsurePresent(); err != nil {
		return err
	}
	dc := &cluster.DockerCLI{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}

	// 2. Render kind.yaml + create cluster (idempotent).
	fmt.Fprintln(out, "[2/7] Rendering kind.yaml + ensuring cluster exists ...")
	kindCfg, err := cluster.RenderKindConfig(cluster.KindConfig{Name: p.Cluster.Name})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repo, "artifacts"), 0o755); err != nil {
		return err
	}
	rendered := filepath.Join(repo, "artifacts", "kind.yaml")
	if err := os.WriteFile(rendered, []byte(kindCfg), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "      rendered → %s\n", rendered)
	exists, err := kc.ClusterExists(ctx, p.Cluster.Name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(out, "      cluster %q already exists — leaving in place\n", p.Cluster.Name)
	} else {
		if err := kc.CreateCluster(ctx, p.Cluster.Name, kindCfg, p.Versions.KindNodeImage); err != nil {
			return err
		}
	}

	// 3. Fetch kubeconfig early — Calico apply uses it.
	fmt.Fprintln(out, "[3/7] Fetching kubeconfig ...")
	kubeconfigPath := filepath.Join(repo, "artifacts", "kubeconfig")
	if err := kc.WriteKubeconfig(ctx, p.Cluster.Name, kubeconfigPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "      %s\n", kubeconfigPath)

	// 4. Apply Calico + NetworkAttachmentDefinition CRD. The NAD CRD
	// is a hard runtime dependency for FLO's manager startup even in
	// demo mode (where no NADs are actually used) — without it, FLO's
	// controller-runtime informers stall and the crd-installer never
	// reconciles the License CRD. We install just the CRD (not the
	// full Multus daemonset) since the kind cluster doesn't actually
	// route through Multus.
	fmt.Fprintln(out, "[4/7] Applying Calico CNI + NetworkAttachmentDefinition CRD ...")
	r := &deploy.Runner{
		KubeconfigPath: kubeconfigPath,
		HelmHome:       filepath.Join(repo, "artifacts", "helm-home"),
		Out:            prefixWriter{w: out, prefix: "      | "},
	}
	if err := r.Kubectl(ctx, "apply", "-f", version.CalicoManifestURL); err != nil {
		return err
	}
	nadCRD, err := embedded.Templates.ReadFile("templates/nad-crd.yaml")
	if err != nil {
		return fmt.Errorf("read embedded nad-crd.yaml: %w", err)
	}
	if err := r.Apply(ctx, string(nadCRD)); err != nil {
		return fmt.Errorf("apply NetworkAttachmentDefinition CRD: %w", err)
	}
	// Wait for Calico controller — gives kindnet replacement enough time
	// that subsequent `kubectl` calls don't race the CNI rollout.
	if err := r.Wait(ctx, "kube-system", "Available", "deployment/calico-kube-controllers",
		5*time.Minute); err != nil {
		fmt.Fprintf(out, "      WARN: calico-kube-controllers not Available in 5min: %v\n", err)
	}

	// 5. Docker networks: create + attach to both nodes.
	fmt.Fprintln(out, "[5/7] Creating + attaching docker networks ...")
	for _, n := range []poc.DockerNetwork{p.Networks.Internal, p.Networks.External} {
		if err := dc.CreateBridgeNetwork(ctx, n.Name, n.Subnet); err != nil {
			return err
		}
	}
	nodes, err := dc.NodeContainers(ctx, p.Cluster.Name)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no kind node containers found for cluster %q — `kind get clusters` says otherwise?", p.Cluster.Name)
	}
	for _, node := range nodes {
		for _, n := range []string{p.Networks.Internal.Name, p.Networks.External.Name} {
			if err := dc.ConnectNetwork(ctx, n, node); err != nil {
				return err
			}
		}
	}

	// 6. Label the worker node for TMM.
	fmt.Fprintln(out, "[6/7] Labelling worker node for TMM ...")
	workerNode := p.Cluster.Name + "-worker"
	labelKey, labelVal := p.BNK.TMMLabel()
	if err := r.Kubectl(ctx, "label", "node", workerNode,
		fmt.Sprintf("%s=%s", labelKey, labelVal), "--overwrite"); err != nil {
		return fmt.Errorf("label %s %s=%s: %w", workerNode, labelKey, labelVal, err)
	}

	// 7. bnk-forge auto-registration (best-effort).
	fmt.Fprintln(out, "[7/7] bnk-forge registration ...")
	if f.skipBNKForge || !p.BNKForge.Enabled {
		fmt.Fprintln(out, "      skipped (disabled or --skip-bnk-forge)")
	} else {
		if err := registerWithBNKForge(ctx, out, repo, p); err != nil {
			if errors.Is(err, bnkforge.ErrNotRunning) {
				fmt.Fprintf(out, "      bnk-forge configured but not running — skipping. (%v)\n", err)
			} else {
				fmt.Fprintf(out, "      WARN: bnk-forge registration failed: %v\n", err)
			}
		}
	}

	p.Status.Cluster = "ready"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	if j, err := appendJournal(repo, "cluster", "cluster up — READY"); err == nil {
		fmt.Fprintf(j, "- cluster: %s\n- nodes: %s\n", p.Cluster.Name, strings.Join(nodes, ", "))
		j.Close()
	}
	fmt.Fprintln(out, "\nDONE.  Next: `kindbnkctl deploy prereqs && deploy flo && deploy cne` (or run e2e).")
	return nil
}

// registerWithBNKForge runs the same flow dpubnkctl's bnk-forge launcher
// uses: ensure running → login → ensure project → register cluster
// with the localized kubeconfig.
func registerWithBNKForge(ctx context.Context, out io.Writer, repo string, p *poc.PoC) error {
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
		desc := fmt.Sprintf("Imported from kindbnkctl PoC %q (BNK %s).",
			p.Metadata.Name, p.Metadata.BNKVersion)
		color := p.BNKForge.ProjectColor
		if color == "" {
			color = "#0a3a5c"
		}
		projectID, err = cli.CreateProject(ctx, bnkforge.Project{
			Name:                  p.Metadata.Name,
			Description:           desc,
			ProjectType:           "kubernetes",
			CloudProvider:         "on-prem",
			Environment:           "dev",
			Region:                p.Metadata.Customer,
			TargetPlatformProfile: "generic_onprem",
			Color:                 color,
			Icon:                  p.BNKForge.ProjectIcon,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "      created bnk-forge project %q (id=%d)\n", p.Metadata.Name, projectID)
	}
	// Cluster registration.
	clusters, err := cli.ListProjectClusters(ctx, projectID)
	if err != nil {
		return err
	}
	for _, c := range clusters {
		if c.Name == p.Metadata.Name {
			fmt.Fprintf(out, "      cluster %q already registered (id=%d)\n", p.Metadata.Name, c.ID)
			return nil
		}
	}
	body, err := os.ReadFile(filepath.Join(repo, "artifacts", "kubeconfig"))
	if err != nil {
		return err
	}
	id, err := cli.CreateProjectCluster(ctx, projectID, bnkforge.Cluster{
		Name:             p.Metadata.Name,
		Kubeconfig:       base64.StdEncoding.EncodeToString(body),
		CloudProvider:    "on-prem",
		Region:           p.Metadata.Customer,
		DefaultNamespace: "default",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      registered cluster %q (id=%d). open %s\n",
		p.Metadata.Name, id, cfg.URL)
	return nil
}
