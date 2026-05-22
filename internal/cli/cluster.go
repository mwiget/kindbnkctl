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

	fmt.Fprintf(out, "PoC:        %s  (BNK %s)\n\n", p.Metadata.Name, p.Metadata.BNKVersion)

	// 1. Container runtime.
	fmt.Fprintln(out, "[1/6] Container-runtime preflight ...")
	rt, err := cluster.Detect(ctx, cluster.Runtime(p.Cluster.Provider))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      using %s\n", rt)

	kc := &cluster.Kind{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}
	if err := kc.EnsurePresent(); err != nil {
		return err
	}

	// 2. Render kind.yaml + create cluster (idempotent).
	fmt.Fprintln(out, "[2/6] Rendering kind.yaml + ensuring cluster exists ...")
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
	fmt.Fprintln(out, "[3/6] Fetching kubeconfig ...")
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
	fmt.Fprintln(out, "[4/6] Applying Calico CNI + NetworkAttachmentDefinition CRD ...")
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

	// 5. Label the worker node for TMM. We dropped the
	// bnk-internal / bnk-external docker bridges that earlier
	// versions of kindbnkctl attached to the kind nodes — no
	// scenario actually consumed them, and the Gateway IP pool
	// (203.0.113.0/24) is plumbed entirely via the bnk-bgp Multus
	// NAD bridge that scenarios create on demand.
	dc := &cluster.DockerCLI{Runtime: rt, Out: prefixWriter{w: out, prefix: "      | "}}
	nodes, err := dc.NodeContainers(ctx, p.Cluster.Name)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no kind node containers found for cluster %q — `kind get clusters` says otherwise?", p.Cluster.Name)
	}
	fmt.Fprintln(out, "[5/6] Labelling worker node for TMM ...")
	workerNode := p.Cluster.Name + "-worker"
	labelKey, labelVal := p.BNK.TMMLabel()
	if err := r.Kubectl(ctx, "label", "node", workerNode,
		fmt.Sprintf("%s=%s", labelKey, labelVal), "--overwrite"); err != nil {
		return fmt.Errorf("label %s %s=%s: %w", workerNode, labelKey, labelVal, err)
	}

	// 6. bnk-forge auto-registration (best-effort).
	fmt.Fprintln(out, "[6/6] bnk-forge registration ...")
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
	// Cluster registration with drift detection: when a cluster row
	// already exists for this PoC name, compare the stored apiserver
	// URL against the localized kubeconfig. If they differ, the row
	// is stale (e.g. destroy + redeploy with the same PoC name; kind
	// rotates the apiserver port on each create) — DELETE the stale
	// row and POST a fresh one so bnk-forge talks to the new cluster.
	clusters, err := cli.ListProjectClusters(ctx, projectID)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(repo, "artifacts", "kubeconfig"))
	if err != nil {
		return err
	}
	localServer, err := bnkforge.KubeconfigAPIServer(body)
	if err != nil {
		return fmt.Errorf("read local kubeconfig: %w", err)
	}
	for _, c := range clusters {
		if c.Name != p.Metadata.Name {
			continue
		}
		if localServer != "" && c.APIServer != "" && c.APIServer == localServer {
			fmt.Fprintf(out, "      cluster %q already registered (id=%d, kubeconfig matches %s)\n",
				p.Metadata.Name, c.ID, c.APIServer)
			return nil
		}
		fmt.Fprintf(out, "      cluster %q stored kubeconfig drifted (stored=%q local=%q) — refreshing registration (id=%d)\n",
			p.Metadata.Name, c.APIServer, localServer, c.ID)
		if err := cli.DeleteCluster(ctx, c.ID); err != nil {
			return fmt.Errorf("delete stale cluster row id=%d: %w", c.ID, err)
		}
		break
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
