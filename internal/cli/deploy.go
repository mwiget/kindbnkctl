package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/deploy"
	"github.com/mwiget/kindbnkctl/internal/poc"
	"github.com/mwiget/kindbnkctl/internal/version"
)

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Install the BNK platform (prereqs, FLO, CNEInstance)",
	}
	cmd.AddCommand(newDeployPrereqsCmd())
	cmd.AddCommand(newDeployFLOCmd())
	cmd.AddCommand(newDeployCNECmd())
	return cmd
}

// ---------- prereqs ----------

type deployPrereqsFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
}

func newDeployPrereqsCmd() *cobra.Command {
	f := &deployPrereqsFlags{}
	cmd := &cobra.Command{
		Use:   "prereqs",
		Short: "Install namespaces + cert-manager + FAR pull secret (DESTRUCTIVE)",
		Long: `Phase deploy-1: install everything BNK needs before the FLO chart:

  1. Tools preflight (kubectl, helm).
  2. Extract FAR pull secret from the tgz at bnk.far_key_ref.
  3. Ensure namespaces: f5-operators, f5-cne-core, default.
  4. Apply far-secret (kubernetes.io/dockerconfigjson) in each.
  5. helm install cert-manager (jetstack), wait for Available.

Required gates:
  --yolo                  acknowledge cluster writes
  --confirm-deploy NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployPrereqs(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	return cmd
}

func runDeployPrereqs(ctx context.Context, out io.Writer, f *deployPrereqsFlags) error {
	repo, p, kubeconfig, err := loadDeployContext(f.pocDir)
	if err != nil {
		return err
	}
	if err := requireTwoGates(f.yolo, "--confirm-deploy", f.confirmDeploy,
		p.Metadata.Name, "deploy prereqs"); err != nil {
		return err
	}
	farPath := resolveRef(repo, p.BNK.FARKeyRef)
	if _, err := os.Stat(farPath); err != nil {
		return fmt.Errorf("FAR tgz not found at %s — drop the file there and retry", farPath)
	}

	r := &deploy.Runner{
		KubeconfigPath: kubeconfig,
		HelmHome:       filepath.Join(repo, "artifacts", "helm-home"),
		Out:            prefixWriter{w: out, prefix: "      | "},
	}

	fmt.Fprintf(out, "PoC:     %s\nCluster: %s\nFAR:     %s\n\n",
		p.Metadata.Name, kubeconfig, farPath)

	fmt.Fprintln(out, "[1/5] Tools preflight ...")
	if err := r.CheckTools(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "      ok")

	fmt.Fprintln(out, "[2/5] Extracting FAR dockerconfigjson ...")
	dockerCfg, err := deploy.ExtractFARDockerConfig(farPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      ok — %d bytes\n", len(dockerCfg))

	fmt.Fprintln(out, "[3/5] Ensuring namespaces ...")
	namespaces := []string{"f5-operators", deploy.SharedComponentNamespace, "default"}
	for _, ns := range namespaces {
		if ns != "default" {
			if err := r.Apply(ctx, deploy.RenderNamespace(ns)); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(out, "      ok")

	fmt.Fprintln(out, "[4/5] Applying far-secret in each namespace ...")
	for _, ns := range namespaces {
		if err := r.Apply(ctx, deploy.RenderFARSecret(ns, dockerCfg)); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	fmt.Fprintln(out, "[5/5] Installing cert-manager ...")
	if err := r.HelmUpgrade(ctx,
		"cert-manager",
		"cert-manager",
		version.CertManagerRepo,
		"cert-manager",
		version.CertManagerVersion,
		"installCRDs: true\n",
	); err != nil {
		return err
	}
	if err := r.Wait(ctx, "cert-manager", "Available", "deployment/cert-manager-webhook", 5*time.Minute); err != nil {
		return fmt.Errorf("cert-manager-webhook not Available: %w", err)
	}
	fmt.Fprintln(out, "      cert-manager Ready.")

	p.Status.Deploy = "in_progress"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nDONE.  Next: `kindbnkctl deploy flo`.")
	return nil
}

// ---------- flo ----------

type deployFLOFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
}

func newDeployFLOCmd() *cobra.Command {
	f := &deployFLOFlags{}
	cmd := &cobra.Command{
		Use:   "flo",
		Short: "Install F5 Lifecycle Operator + CWC API certs (DESTRUCTIVE)",
		Long: `Phase deploy-2: install FLO:

  1. Pull release-manifest from repo.f5.com to resolve FLO + cert-gen
     chart versions for BNK ` + version.CNEManifestVersion + `.
  2. Inspect JWT (diagnostic — TEEM endpoint is derived from the JWT's
     jku at runtime by CWC).
  3. Apply bnk-ca cert-issuer chain.
  4. helm install FLO (manifest-resolved version).
  5. Wait for FLO controller Available.
  6. Generate + apply CWC API certs (gen_cert.sh in alpine/k8s).

Required gates:
  --yolo                  acknowledge cluster writes
  --confirm-deploy NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployFLO(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	return cmd
}

func runDeployFLO(ctx context.Context, out io.Writer, f *deployFLOFlags) error {
	repo, p, kubeconfig, err := loadDeployContext(f.pocDir)
	if err != nil {
		return err
	}
	if err := requireTwoGates(f.yolo, "--confirm-deploy", f.confirmDeploy,
		p.Metadata.Name, "deploy flo"); err != nil {
		return err
	}
	farPath := resolveRef(repo, p.BNK.FARKeyRef)
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	for _, x := range []struct{ what, path string }{
		{"FAR tgz", farPath}, {"JWT", jwtPath},
	} {
		if _, err := os.Stat(x.path); err != nil {
			return fmt.Errorf("%s not found at %s — drop the file there and retry", x.what, x.path)
		}
	}

	r := &deploy.Runner{
		KubeconfigPath: kubeconfig,
		HelmHome:       filepath.Join(repo, "artifacts", "helm-home"),
		Out:            prefixWriter{w: out, prefix: "      | "},
	}

	fmt.Fprintf(out, "PoC:      %s\nCluster:  %s\nJWT:      %s\nManifest: %s\n\n",
		p.Metadata.Name, kubeconfig, jwtPath, version.CNEManifestVersion)

	// 1. Pull release-manifest.
	fmt.Fprintln(out, "[1/6] Pulling release-manifest from repo.f5.com ...")
	farAuth, err := deploy.ExtractFARRegistryAuth(farPath)
	if err != nil {
		return fmt.Errorf("extract FAR registry creds: %w", err)
	}
	mfCache := filepath.Join(repo, "artifacts", "release-manifest")
	manifest, err := deploy.PullReleaseManifest(ctx, farAuth, version.CNEManifestVersion, mfCache,
		filepath.Join(repo, "artifacts", "helm-home"))
	if err != nil {
		return fmt.Errorf("pull release-manifest: %w", err)
	}
	floVer := manifest.Chart("charts/f5-lifecycle-operator")
	certGenVer := manifest.Chart("utils/f5-cert-gen")
	if floVer == "" {
		return fmt.Errorf("release-manifest %s has no charts/f5-lifecycle-operator", manifest.Version)
	}
	if certGenVer == "" {
		return fmt.Errorf("release-manifest %s has no utils/f5-cert-gen", manifest.Version)
	}
	fmt.Fprintf(out, "      FLO chart    %s\n", floVer)
	fmt.Fprintf(out, "      f5-cert-gen  %s\n", certGenVer)
	p.Versions.FLOChart = floVer
	if err := savePoC(repo, p, out); err != nil {
		return err
	}

	// 2. JWT inspection.
	fmt.Fprintln(out, "[2/6] Inspecting JWT (diagnostic) ...")
	info, err := deploy.InspectJWT(jwtPath)
	if err != nil {
		return err
	}
	if _, err := readJWT(jwtPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "      type=%s  jku=%s  sub=%s\n", info.Type, info.JKU, info.Sub)

	// 3. bnk-ca cert-issuer chain.
	fmt.Fprintln(out, "[3/6] Applying bnk-ca cert-issuer chain ...")
	if err := r.Apply(ctx, deploy.CertIssuerChain()); err != nil {
		return err
	}
	if err := r.Wait(ctx, "cert-manager", "Ready", "certificate/bnk-ca", 3*time.Minute); err != nil {
		return fmt.Errorf("bnk-ca cert not ready: %w", err)
	}

	// 4. FLO helm install.
	fmt.Fprintln(out, "[4/6] Rendering FLO values + helm install ...")
	values, err := deploy.RenderFLOValues(deploy.FLOInputs{
		Namespace:                "f5-operators",
		SharedComponentNamespace: deploy.SharedComponentNamespace,
		ClusterIssuer:            "bnk-ca-cluster-issuer",
	})
	if err != nil {
		return err
	}
	rendered := filepath.Join(repo, "artifacts", "flo-values-rendered.yaml")
	if err := os.WriteFile(rendered, []byte(values), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "      rendered → %s\n", rendered)
	if err := r.HelmUpgradeOCI(ctx, farAuth,
		"flo", version.FLOChartOCIRef,
		"f5-operators", floVer, values,
	); err != nil {
		return err
	}

	// 5. Wait for FLO.
	fmt.Fprintln(out, "[5/6] Waiting for FLO controller Available ...")
	if err := r.Wait(ctx, "f5-operators", "Available", "deployment", 5*time.Minute,
		"-l", "app.kubernetes.io/name=f5-lifecycle-operator"); err != nil {
		fmt.Fprintf(out, "      WARN: FLO not Ready within 5min: %v\n", err)
	} else {
		fmt.Fprintln(out, "      FLO controller Ready.")
	}

	// 6. CWC API certs.
	fmt.Fprintln(out, "[6/6] Generating + applying CWC API certs ...")
	certsWork := filepath.Join(repo, "artifacts", "cwc-certs")
	if err := deploy.PullAndApplyCWCCerts(ctx, r, farAuth, certGenVer, certsWork,
		deploy.SharedComponentNamespace,
		prefixWriter{w: out, prefix: "      | "}); err != nil {
		return fmt.Errorf("CWC cert preflight: %w", err)
	}
	fmt.Fprintln(out, "      cwc-license-certs + cwc-license-client-certs applied.")

	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nDONE.  Next: `kindbnkctl deploy cne`.")
	return nil
}

// ---------- cne ----------

type deployCNEFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
}

func newDeployCNECmd() *cobra.Command {
	f := &deployCNEFlags{}
	cmd := &cobra.Command{
		Use:   "cne",
		Short: "Apply CNEInstance + License CR (DESTRUCTIVE)",
		Long: `Phase deploy-3: apply the workload custom resources:

  1. Apply CNEInstance (demoMode=true, TMM pinned via nodeSelector).
  2. Apply License CR with the operator's JWT.
  3. Wait for License to become Active (or warn).

Required gates:
  --yolo                  acknowledge cluster writes
  --confirm-deploy NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployCNE(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	return cmd
}

func runDeployCNE(ctx context.Context, out io.Writer, f *deployCNEFlags) error {
	repo, p, kubeconfig, err := loadDeployContext(f.pocDir)
	if err != nil {
		return err
	}
	if err := requireTwoGates(f.yolo, "--confirm-deploy", f.confirmDeploy,
		p.Metadata.Name, "deploy cne"); err != nil {
		return err
	}
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	jwt, err := readJWT(jwtPath)
	if err != nil {
		return fmt.Errorf("read JWT: %w", err)
	}

	r := &deploy.Runner{
		KubeconfigPath: kubeconfig,
		HelmHome:       filepath.Join(repo, "artifacts", "helm-home"),
		Out:            prefixWriter{w: out, prefix: "      | "},
	}

	fmt.Fprintf(out, "PoC:     %s\nCluster: %s\n\n", p.Metadata.Name, kubeconfig)

	// 1. CNEInstance.
	fmt.Fprintln(out, "[1/3] Rendering + applying CNEInstance (demoMode=true) ...")
	cne, err := deploy.RenderCNEInstance(p)
	if err != nil {
		return err
	}
	rendered := filepath.Join(repo, "artifacts", "cne-instance-rendered.yaml")
	if err := os.WriteFile(rendered, []byte(cne), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "      rendered → %s\n", rendered)
	if err := r.Apply(ctx, cne); err != nil {
		return err
	}

	// 2. License CR. The licenses.k8s.f5net.com CRD is installed by
	// FLO's crd-installer in response to the CNEInstance we just
	// applied — empirically, it doesn't exist until then. Wait for
	// both --for=create (CRD object exists) and condition=Established
	// (apiserver has bound the schema) before applying.
	fmt.Fprintln(out, "[2/3] Waiting for license CRD, then applying License CR ...")
	if err := r.Kubectl(ctx, "wait", "--for=create",
		"crd/licenses.k8s.f5net.com", "--timeout=3m"); err != nil {
		return fmt.Errorf("license CRD never created (FLO did not reconcile?): %w", err)
	}
	if err := r.Wait(ctx, "", "Established",
		"crd/licenses.k8s.f5net.com", 3*time.Minute); err != nil {
		return fmt.Errorf("license CRD did not become Established: %w", err)
	}
	cr, err := deploy.RenderLicenseCR(deploy.LicenseInputs{JWT: jwt})
	if err != nil {
		return err
	}
	// CWC creates a ResourceQuota (f5-single-license-quota) when it
	// reconciles License; for a brief window, `kubectl apply` fails
	// with "status unknown for quota". Retry up to ~100s.
	if err := applyLicenseWithQuotaRetry(ctx, r, cr, out); err != nil {
		return fmt.Errorf("apply License CR: %w", err)
	}

	// 3. Wait for License Active.
	fmt.Fprintln(out, "[3/3] Waiting for License Active ...")
	if err := deploy.WaitForLicenseActive(ctx, r,
		deploy.LicenseCRName, deploy.SharedComponentNamespace,
		20*time.Minute); err != nil {
		if strings.Contains(err.Error(), "PendingVerification") {
			fmt.Fprintf(out, "      WARN: license stuck at PendingVerification (disconnected mode? follow F5 docs to register manually)\n")
		} else {
			fmt.Fprintf(out, "      WARN: %v\n", err)
		}
	} else {
		fmt.Fprintln(out, "      License Active.")
	}

	p.Status.Deploy = "ready"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	if j, jerr := appendJournal(repo, "deploy", "deploy cne — APPLIED"); jerr == nil {
		fmt.Fprintf(j, "- CNEInstance + License applied at %s\n", time.Now().UTC().Format(time.RFC3339))
		j.Close()
	}
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// applyLicenseWithQuotaRetry retries `kubectl apply` while the
// f5-single-license-quota's status.used is still unpopulated (CWC
// creates the quota at the same time it reconciles License, and the
// quota controller takes a moment to populate it). Bounded retry:
// 20 attempts × 5s = ~100s.
func applyLicenseWithQuotaRetry(ctx context.Context, r *deploy.Runner, manifest string, out io.Writer) error {
	const attempts = 20
	const interval = 5 * time.Second
	const quotaName = "f5-single-license-quota"
	var lastErr error
	for i := 1; i <= attempts; i++ {
		lastErr = r.Apply(ctx, manifest)
		if lastErr == nil {
			return nil
		}
		msg := lastErr.Error()
		if !strings.Contains(msg, "status unknown for quota") || !strings.Contains(msg, quotaName) {
			return lastErr
		}
		fmt.Fprintf(out, "      quota %s status not yet computed, retrying in %s (attempt %d/%d) ...\n",
			quotaName, interval, i, attempts)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("quota %s status never populated: %w", quotaName, lastErr)
}

// loadDeployContext resolves the PoC, loads it, and confirms the
// kubeconfig is present. Used by all three deploy subcommands.
func loadDeployContext(pocDir string) (string, *poc.PoC, string, error) {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return "", nil, "", err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return "", nil, "", fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	kc, err := requireKubeconfig(repo, "run `kindbnkctl cluster up` first")
	if err != nil {
		return "", nil, "", err
	}
	return repo, p, kc, nil
}
