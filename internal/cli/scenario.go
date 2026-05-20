package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/deploy"
	"github.com/mwiget/kindbnkctl/internal/poc"
	"github.com/mwiget/kindbnkctl/internal/scenarios"

	// Side-effect imports: each blank-imported package registers its
	// scenario(s) with internal/scenarios at init time. Add new ones
	// here as they land.
	_ "github.com/mwiget/kindbnkctl/internal/scenarios/bgppeer"
	_ "github.com/mwiget/kindbnkctl/internal/scenarios/extrespool"
	_ "github.com/mwiget/kindbnkctl/internal/scenarios/httproutee2e"
	_ "github.com/mwiget/kindbnkctl/internal/scenarios/proxyprotocol"
)

func newScenarioCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scenario",
		Short: "Run F5 BNK how-to scenarios against the running cluster",
		Long: `Each scenario maps to one F5 how-to article from
clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ and
exercises a slice of BNK functionality end-to-end: render manifests
under artifacts/scenarios/<name>/, apply them, assert the reconciled
state, write a report under reports/<timestamp>/scenarios/<name>.json.

Rating tells you whether the scenario can actually run in the
kindbnkctl 2-node / demo-mode TMM shape:

  green   fully testable here
  amber   partially testable (some assertions skipped or relaxed)
  red     not testable; listed for discoverability, never executed`,
	}
	cmd.AddCommand(
		newScenarioListCmd(),
		newScenarioRunCmd(),
		newScenarioCleanCmd(),
	)
	return cmd
}

func newScenarioListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all known scenarios + their rating",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			items := scenarios.All()
			sort.Slice(items, func(i, j int) bool { return items[i].Name() < items[j].Name() })
			fmt.Fprintf(out, "%-22s %-7s %-22s %s\n", "NAME", "RATING", "DEPENDS-ON", "TITLE")
			for _, s := range items {
				deps := strings.Join(s.Dependencies(), ",")
				if deps == "" {
					deps = "-"
				}
				fmt.Fprintf(out, "%-22s %-7s %-22s %s\n", s.Name(), s.Rating(), deps, s.Title())
			}
			if len(items) == 0 {
				fmt.Fprintln(out, "(no scenarios registered)")
			}
			return nil
		},
	}
}

type scenarioRunFlags struct {
	pocDir  string
	all     bool
	dryRun  bool
	verbose bool
}

func newScenarioRunCmd() *cobra.Command {
	f := &scenarioRunFlags{}
	cmd := &cobra.Command{
		Use:   "run [name]",
		Short: "Run one scenario (or --all green-rated) against the cluster",
		Long: `Run a single scenario by name, or use --all to run every
green-rated scenario in registration order. Red-rated scenarios are
always skipped, even with --all.

Manifests are rendered into artifacts/scenarios/<name>/ before any
cluster I/O. With --dry-run the rendered files are written but
nothing is applied — handy to inspect what would land.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenarios(cmd.Context(), cmd.OutOrStdout(), args, f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.all, "all", false, "Run every green-rated scenario in registration order")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Render manifests but apply nothing")
	cmd.Flags().BoolVar(&f.verbose, "verbose", false, "Surface per-assertion lines + Details to stdout (always in the JSON report)")
	return cmd
}

func runScenarios(ctx context.Context, out io.Writer, args []string, f *scenarioRunFlags) error {
	if !f.all && len(args) != 1 {
		return fmt.Errorf("provide a scenario name OR --all (see `kindbnkctl scenario list`)")
	}
	if f.all && len(args) > 0 {
		return fmt.Errorf("--all and a positional name are mutually exclusive")
	}

	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	kubeconfig, err := requireKubeconfig(repo, "run `kindbnkctl cluster up` first")
	if err != nil {
		return err
	}

	sctx := &scenarios.Context{
		Ctx:    ctx,
		PoC:    p,
		PoCDir: repo,
		Runner: &deploy.Runner{
			KubeconfigPath: kubeconfig,
			HelmHome:       repo + "/artifacts/helm-home",
			Out:            prefixWriter{w: out, prefix: "      | "},
		},
		Out:     out,
		DryRun:  f.dryRun,
		Verbose: f.verbose,
	}

	var todo []scenarios.Scenario
	if f.all {
		for _, s := range scenarios.All() {
			if s.Rating() == scenarios.Green {
				todo = append(todo, s)
			}
		}
		if len(todo) == 0 {
			fmt.Fprintln(out, "no green-rated scenarios registered")
			return nil
		}
	} else {
		s := scenarios.Find(args[0])
		if s == nil {
			return fmt.Errorf("unknown scenario %q (see `kindbnkctl scenario list`)", args[0])
		}
		todo = append(todo, s)
	}

	failed := 0
	for _, s := range todo {
		r := scenarios.Run(sctx, s)
		if r.Status == "failed" {
			failed++
		}
		fmt.Fprintln(out)
	}
	if failed > 0 {
		return fmt.Errorf("%d scenario(s) failed", failed)
	}
	return nil
}

func newScenarioCleanCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "clean [name]",
		Short: "Delete the cluster objects a scenario applied",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolvePoCDir(pocDir)
			if err != nil {
				return err
			}
			p, err := poc.Load(repo)
			if err != nil {
				return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
			}
			kubeconfig, err := requireKubeconfig(repo, "run `kindbnkctl cluster up` first")
			if err != nil {
				return err
			}
			s := scenarios.Find(args[0])
			if s == nil {
				return fmt.Errorf("unknown scenario %q (see `kindbnkctl scenario list`)", args[0])
			}
			sctx := &scenarios.Context{
				Ctx:    cmd.Context(),
				PoC:    p,
				PoCDir: repo,
				Runner: &deploy.Runner{
					KubeconfigPath: kubeconfig,
					HelmHome:       repo + "/artifacts/helm-home",
					Out:            prefixWriter{w: cmd.OutOrStdout(), prefix: "      | "},
				},
				Out: cmd.OutOrStdout(),
			}
			return scenarios.Cleanup(sctx, s)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

