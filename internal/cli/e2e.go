package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/deploy"
	"github.com/mwiget/kindbnkctl/internal/poc"
	"github.com/mwiget/kindbnkctl/internal/scenarios"
	"github.com/mwiget/kindbnkctl/internal/version"
)

// e2ePhase is one orchestrated step.
type e2ePhase struct {
	name        string
	subcmd      []string
	destructive bool
	confirmFlag string // e.g. --confirm-cluster | --confirm-deploy
}

var canonicalPhases = []e2ePhase{
	{name: "validate", subcmd: []string{"validate"}},
	{name: "cluster-up", subcmd: []string{"cluster", "up"},
		destructive: true, confirmFlag: "--confirm-cluster"},
	{name: "deploy-prereqs", subcmd: []string{"deploy", "prereqs"},
		destructive: true, confirmFlag: "--confirm-deploy"},
	{name: "deploy-flo", subcmd: []string{"deploy", "flo"},
		destructive: true, confirmFlag: "--confirm-deploy"},
	{name: "deploy-cne", subcmd: []string{"deploy", "cne"},
		destructive: true, confirmFlag: "--confirm-deploy"},
}

type e2eFlags struct {
	pocDir            string
	phaseFilter       string
	reportDir         string
	yolo              bool
	dryRun            bool
	continueOnFailure bool
	noResume          bool
	confirmCluster    string
	withScenarios     bool
}

func newE2ECmd() *cobra.Command {
	f := &e2eFlags{}
	cmd := &cobra.Command{
		Use:   "e2e",
		Short: "Drive the full deploy pipeline end-to-end (validate → cluster up → deploy)",
		Long: `Run every phase from validate through deploy cne in order,
auto-filling --yolo and --confirm-* safety gates from poc.yaml. Each
phase's stdout/stderr lands at reports/<timestamp>/logs/NN-<phase>.log;
per-phase results aggregate into reports/<timestamp>/run.{json,md}.

Phases:

  validate           kindbnkctl validate
  cluster-up         kindbnkctl cluster up --yolo --confirm-cluster <name>
  deploy-prereqs     kindbnkctl deploy prereqs --yolo --confirm-deploy <name>
  deploy-flo         kindbnkctl deploy flo --yolo --confirm-deploy <name>
  deploy-cne         kindbnkctl deploy cne --yolo --confirm-deploy <name>

Invocation:

  kindbnkctl e2e                       Print the plan (no-op).
  kindbnkctl e2e --dry-run             Show the exact per-phase invocations.
  kindbnkctl e2e --yolo                Actually run (resume-safe via
                                        artifacts/e2e-state.json).
  kindbnkctl e2e --yolo --no-resume    Re-run every phase from scratch.
  kindbnkctl e2e --yolo --phase A,B,C  Only the listed phases.
  kindbnkctl e2e --yolo --with-scenarios
                                       After deploy succeeds, run every
                                        green scenario and roll the results
                                        into the same run.{json,md} so one
                                        report covers cluster bring-up
                                        + every how-to.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runE2E(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.phaseFilter, "phase", "", "Comma-separated subset of phases")
	cmd.Flags().StringVar(&f.reportDir, "report-dir", "", "Output dir (default: <poc>/reports/<timestamp>/)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge destructive; required to actually run")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the plan, run nothing")
	cmd.Flags().BoolVar(&f.continueOnFailure, "continue-on-failure", false, "Keep running after a phase fails")
	cmd.Flags().BoolVar(&f.noResume, "no-resume", false, "Ignore artifacts/e2e-state.json")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Required typo-guard; must equal poc.yaml.metadata.name. Also used for --confirm-deploy")
	cmd.Flags().BoolVar(&f.withScenarios, "with-scenarios", false, "After deploy succeeds, run every green scenario (topo-sorted) and include results in the same run.{json,md}")
	return cmd
}

type e2eState struct {
	Phases map[string]e2ePhaseState `json:"phases"`
}

type e2ePhaseState struct {
	Status      string    `json:"status"`
	CompletedAt time.Time `json:"completed_at"`
	Duration    string    `json:"duration,omitempty"`
}

func loadE2EState(repo string) e2eState {
	s := e2eState{Phases: map[string]e2ePhaseState{}}
	path := filepath.Join(repo, "artifacts", "e2e-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	if s.Phases == nil {
		s.Phases = map[string]e2ePhaseState{}
	}
	return s
}

func saveE2EState(repo string, s e2eState) error {
	dir := filepath.Join(repo, "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(filepath.Join(dir, "e2e-state.json"), data, 0o644)
}

func runE2E(ctx context.Context, out io.Writer, f *e2eFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	selected, err := selectPhases(f.phaseFilter)
	if err != nil {
		return err
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate kindbnkctl binary: %w", err)
	}

	// --yolo gate: bare `e2e` prints the plan and exits.
	if !f.yolo && !f.dryRun {
		printPlan(out, p, repo, binary, selected)
		return nil
	}

	// When --yolo is set, --confirm-cluster is required (mirrors per-
	// command gates; e2e itself just forwards the value).
	if f.yolo {
		if f.confirmCluster == "" {
			return fmt.Errorf("--confirm-cluster is required with --yolo (must equal poc.yaml.metadata.name = %q)", p.Metadata.Name)
		}
		if f.confirmCluster != p.Metadata.Name {
			return fmt.Errorf("--confirm-cluster mismatch: got %q, expected %q", f.confirmCluster, p.Metadata.Name)
		}
	}
	// In dry-run mode (no --yolo) we still want the printed commands
	// to be copy-pasteable, so default confirm to the PoC name.
	if f.dryRun && f.confirmCluster == "" {
		f.confirmCluster = p.Metadata.Name
	}

	reportDir := f.reportDir
	if reportDir == "" {
		reportDir = filepath.Join(repo, "reports", time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	}
	logDir := filepath.Join(reportDir, "logs")
	if !f.dryRun {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "PoC: %s   (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
	fmt.Fprintf(out, "Phases (%d): %s\n", len(selected), phaseNames(selected))
	if !f.dryRun {
		fmt.Fprintf(out, "Reports: %s\n", reportDir)
	}
	fmt.Fprintln(out)

	report := runReport{
		StartedAt: time.Now().UTC(),
		PoCName:   p.Metadata.Name,
	}
	state := loadE2EState(repo)
	if f.noResume {
		state = e2eState{Phases: map[string]e2ePhaseState{}}
	}

	for i, ph := range selected {
		idx := i + 1
		stepName := fmt.Sprintf("%02d-%s", idx, ph.name)
		if !f.dryRun && ph.name != "validate" {
			if prev, ok := state.Phases[ph.name]; ok && prev.Status == "ok" {
				reason := fmt.Sprintf("resumed: previously completed at %s",
					prev.CompletedAt.Format(time.RFC3339))
				fmt.Fprintf(out, "[%d/%d] %-18s  SKIPPED — %s\n", idx, len(selected), ph.name, reason)
				report.Phases = append(report.Phases, phaseReport{
					Phase: ph.name, Status: "skipped", Summary: reason, Index: idx,
				})
				continue
			}
		}

		args := buildArgs(p, repo, ph, f.confirmCluster)
		shown := binary + " " + strings.Join(args, " ")
		fmt.Fprintf(out, "[%d/%d] %s\n      %s\n", idx, len(selected), ph.name, shown)

		if f.dryRun {
			report.Phases = append(report.Phases, phaseReport{
				Phase: ph.name, Status: "dry-run", Summary: shown, Index: idx,
			})
			continue
		}

		started := time.Now()
		logPath := filepath.Join(logDir, stepName+".log")
		exit, runErr := runOnePhase(ctx, binary, args, logPath)
		dur := time.Since(started)
		rep := phaseReport{
			Phase:     ph.name,
			Index:     idx,
			StartedAt: started.UTC(),
			Duration:  dur.Truncate(time.Second).String(),
			ExitCode:  exit,
			LogPath:   "logs/" + stepName + ".log",
			Command:   shown,
		}
		switch {
		case runErr != nil && exit < 0:
			rep.Status = "failed"
			rep.Summary = "transport error: " + runErr.Error()
		case exit != 0:
			rep.Status = "failed"
			rep.Summary = fmt.Sprintf("exit %d (see %s)", exit, rep.LogPath)
		default:
			rep.Status = "ok"
			rep.Summary = "completed in " + rep.Duration
		}
		fmt.Fprintf(out, "      %s  (%s, %s)\n\n", strings.ToUpper(rep.Status), rep.Duration, rep.LogPath)
		report.Phases = append(report.Phases, rep)

		if ph.name != "validate" {
			state.Phases[ph.name] = e2ePhaseState{
				Status:      rep.Status,
				CompletedAt: time.Now().UTC(),
				Duration:    rep.Duration,
			}
			if err := saveE2EState(repo, state); err != nil {
				fmt.Fprintf(out, "      WARN: persist e2e state: %v\n", err)
			}
		}
		if rep.Status == "failed" && !f.continueOnFailure {
			break
		}
	}

	// Count deploy failures BEFORE the optional scenarios pass so we
	// don't bother running scenarios against a half-deployed cluster.
	deployFailed := 0
	for _, ph := range report.Phases {
		if ph.Status == "failed" {
			deployFailed++
		}
	}

	if f.withScenarios && !f.dryRun && deployFailed == 0 {
		if err := runScenariosForE2E(ctx, out, repo, reportDir, &report); err != nil {
			fmt.Fprintf(out, "WARN: scenario phase: %v\n", err)
		}
	} else if f.withScenarios && deployFailed > 0 {
		fmt.Fprintln(out, "(skipping --with-scenarios because deploy failed)")
	}

	report.FinishedAt = time.Now().UTC()
	if f.dryRun {
		return nil
	}
	if err := writeRunReports(reportDir, report); err != nil {
		fmt.Fprintf(out, "WARN: write reports: %v\n", err)
	}
	scenarioFailed := 0
	for _, s := range report.Scenarios {
		if s.Status == "failed" {
			scenarioFailed++
		}
	}
	if deployFailed > 0 {
		return fmt.Errorf("e2e: %d phase(s) failed — see %s", deployFailed, reportDir)
	}
	if scenarioFailed > 0 {
		return fmt.Errorf("e2e: %d scenario(s) failed — see %s", scenarioFailed, reportDir)
	}
	fmt.Fprintf(out, "DONE. Report at %s\n", reportDir)
	return nil
}

// runScenariosForE2E runs every green scenario in topo-sorted order
// against the freshly-deployed cluster and appends one SummaryEntry
// per scenario to report.Scenarios. Per-scenario JSON files land in
// the same reports/<stamp>/scenarios/ dir as the e2e run.{json,md}
// (driven by sctx.ReportStamp).
func runScenariosForE2E(ctx context.Context, out io.Writer, repo, reportDir string, report *runReport) error {
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("reload PoC: %w", err)
	}
	kubeconfig, err := requireKubeconfig(repo, "deploy did not produce a kubeconfig")
	if err != nil {
		return err
	}

	// The reports dir name is the timestamp portion of reportDir.
	// We pass that through so per-scenario JSONs share the e2e tree.
	stamp := filepath.Base(strings.TrimRight(filepath.Clean(reportDir), "/"))

	sctx := &scenarios.Context{
		Ctx:    ctx,
		PoC:    p,
		PoCDir: repo,
		Runner: &deploy.Runner{
			KubeconfigPath: kubeconfig,
			HelmHome:       filepath.Join(repo, "artifacts", "helm-home"),
			Out:            prefixWriter{w: out, prefix: "      | "},
		},
		Out:         out,
		ReportStamp: stamp,
	}

	// Same green-only + topo-sort policy as `scenario run --all`.
	var greens []scenarios.Scenario
	for _, s := range scenarios.All() {
		if s.Rating() == scenarios.Green {
			greens = append(greens, s)
		}
	}
	if len(greens) == 0 {
		fmt.Fprintln(out, "no green-rated scenarios registered")
		return nil
	}
	ordered, err := topoSortByDeps(greens)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\n--with-scenarios: running %d green scenario(s) ...\n\n", len(ordered))
	for _, s := range ordered {
		r := scenarios.Run(sctx, s)
		report.Scenarios = append(report.Scenarios, scenarios.SummaryEntry{
			Name:    s.Name(),
			Rating:  string(s.Rating()),
			Status:  r.Status,
			Summary: r.Summary,
		})
		fmt.Fprintln(out)
	}
	return nil
}

func printPlan(out io.Writer, p *poc.PoC, repo, binary string, selected []e2ePhase) {
	fmt.Fprintf(out, "PoC: %s   (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
	fmt.Fprintf(out, "Repo: %s\n\n", repo)
	fmt.Fprintln(out, "`kindbnkctl e2e` runs the full deploy pipeline end-to-end. It is")
	fmt.Fprintln(out, "DESTRUCTIVE (kind create cluster, helm installs, CR applies) and")
	fmt.Fprintln(out, "typically takes 10–20 minutes with a warm Docker cache.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Phases (%d) — would run, in order:\n\n", len(selected))
	for i, ph := range selected {
		args := buildArgs(p, repo, ph, p.Metadata.Name)
		fmt.Fprintf(out, "  %d. %-15s %s %s\n", i+1, ph.name, binary, strings.Join(args, " "))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Nothing has been changed. To proceed:")
	fmt.Fprintf(out, "  kindbnkctl e2e --yolo --confirm-cluster %s\n", p.Metadata.Name)
}

func selectPhases(filter string) ([]e2ePhase, error) {
	if filter == "" {
		return canonicalPhases, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(filter, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var out []e2ePhase
	for _, ph := range canonicalPhases {
		if want[ph.name] {
			out = append(out, ph)
			delete(want, ph.name)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for k := range want {
			unknown = append(unknown, k)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown phase(s): %s (valid: %s)",
			strings.Join(unknown, ", "), phaseNames(canonicalPhases))
	}
	return out, nil
}

func phaseNames(phs []e2ePhase) string {
	names := make([]string, len(phs))
	for i, ph := range phs {
		names[i] = ph.name
	}
	return strings.Join(names, ", ")
}

func buildArgs(p *poc.PoC, repo string, ph e2ePhase, confirmVal string) []string {
	args := append([]string{}, ph.subcmd...)
	args = append(args, "--poc", repo)
	if ph.destructive {
		args = append(args, "--yolo", ph.confirmFlag, confirmVal)
	}
	return args
}

func runOnePhase(ctx context.Context, binary string, args []string, logPath string) (int, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

type runReport struct {
	PoCName    string                     `json:"poc_name"`
	StartedAt  time.Time                  `json:"started_at"`
	FinishedAt time.Time                  `json:"finished_at"`
	Phases     []phaseReport              `json:"phases"`
	Scenarios  []scenarios.SummaryEntry   `json:"scenarios,omitempty"`
}

type phaseReport struct {
	Index     int       `json:"index"`
	Phase     string    `json:"phase"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Duration  string    `json:"duration,omitempty"`
	ExitCode  int       `json:"exit_code,omitempty"`
	LogPath   string    `json:"log_path,omitempty"`
	Command   string    `json:"command,omitempty"`
	Summary   string    `json:"summary"`
}

func writeRunReports(dir string, r runReport) error {
	jb, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "run.json"), jb, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "run.md"), []byte(renderRunMarkdown(r)), 0o644)
}

func renderRunMarkdown(r runReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# e2e report — %s\n\n", r.PoCName)
	fmt.Fprintf(&b, "- **Started:** %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Finished:** %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Wall:** %s\n", r.FinishedAt.Sub(r.StartedAt).Truncate(time.Second))
	fmt.Fprintf(&b, "- **kindbnkctl:** %s (BNK %s, manifest %s)\n\n",
		version.Version, version.BNKVersion, version.CNEManifestVersion)
	ok, failed, skipped := 0, 0, 0
	for _, ph := range r.Phases {
		switch ph.Status {
		case "ok":
			ok++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	fmt.Fprintf(&b, "**Result:** %d ok, %d failed, %d skipped\n\n", ok, failed, skipped)
	b.WriteString("## Deploy phases\n\n")
	b.WriteString("| # | Phase | Status | Duration | Log |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, ph := range r.Phases {
		dur := ph.Duration
		if dur == "" {
			dur = "—"
		}
		lg := "—"
		if ph.LogPath != "" {
			lg = "`" + ph.LogPath + "`"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n",
			ph.Index, ph.Phase, ph.Status, dur, lg)
	}

	if len(r.Scenarios) > 0 {
		sOK, sFailed, sSkipped := 0, 0, 0
		for _, s := range r.Scenarios {
			switch s.Status {
			case "ok":
				sOK++
			case "failed":
				sFailed++
			case "skipped":
				sSkipped++
			}
		}
		fmt.Fprintf(&b, "\n## Scenarios\n\n%d ok, %d failed, %d skipped\n\n",
			sOK, sFailed, sSkipped)
		b.WriteString("| Scenario | Rating | Status | Summary |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, s := range r.Scenarios {
			fmt.Fprintf(&b, "| [`%s`](scenarios/%s.json) | %s | %s | %s |\n",
				s.Name, s.Name, s.Rating, s.Status, mdEscapeBar(s.Summary))
		}
	}
	return b.String()
}

func mdEscapeBar(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}
