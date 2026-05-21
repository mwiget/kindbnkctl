package scenarios

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run drives one scenario through Manifests → Apply → Verify and
// persists a report at <PoCDir>/reports/<timestamp>/scenarios/<name>.json.
// Returns the Result so the caller (CLI) can set the process exit code.
func Run(ctx *Context, s Scenario) Result {
	started := time.Now()
	fmt.Fprintf(ctx.Out, "scenario:  %s  (%s)\n", s.Name(), s.Rating())
	fmt.Fprintf(ctx.Out, "title:     %s\n\n", s.Title())

	if s.Rating() == Red {
		r := Result{
			Status:  "skipped",
			Summary: "rated red — not testable in the kindbnkctl 2-node / demo-TMM shape",
			Details: s.Description(),
		}
		writeReport(ctx.PoCDir, ctx.ReportStamp, s.Name(), r, started)
		fmt.Fprintln(ctx.Out, "SKIPPED — see report")
		return r
	}

	fmt.Fprintln(ctx.Out, "[1/3] Rendering manifests ...")
	paths, err := s.Manifests(ctx)
	if err != nil {
		return finalize(ctx, s, started, Result{
			Status:  "failed",
			Summary: "render: " + err.Error(),
		})
	}
	for _, p := range paths {
		fmt.Fprintf(ctx.Out, "      %s\n", p)
	}

	if ctx.DryRun {
		r := Result{
			Status:   "dry-run",
			Summary:  fmt.Sprintf("%d manifest(s) rendered; nothing applied", len(paths)),
			Manifest: strings.Join(paths, ","),
		}
		writeReport(ctx.PoCDir, ctx.ReportStamp, s.Name(), r, started)
		return r
	}

	fmt.Fprintln(ctx.Out, "[2/3] Applying ...")
	if err := s.Apply(ctx); err != nil {
		return finalize(ctx, s, started, Result{
			Status:  "failed",
			Summary: "apply: " + err.Error(),
		})
	}

	fmt.Fprintln(ctx.Out, "[3/3] Verifying ...")
	r := s.Verify(ctx)
	r.Manifest = strings.Join(paths, ",")
	return finalize(ctx, s, started, r)
}

func finalize(ctx *Context, s Scenario, started time.Time, r Result) Result {
	writeReport(ctx.PoCDir, ctx.ReportStamp, s.Name(), r, started)
	if ctx.Verbose && len(r.Assertions) > 0 {
		for _, a := range r.Assertions {
			mark := "✓"
			if !a.OK {
				mark = "✗"
			}
			line := fmt.Sprintf("      %s %s", mark, a.Description)
			if a.Got != "" {
				line += "  (" + a.Got + ")"
			}
			fmt.Fprintln(ctx.Out, line)
		}
	}
	fmt.Fprintf(ctx.Out, "      %s — %s\n", strings.ToUpper(r.Status), r.Summary)
	if r.Details != "" && (ctx.Verbose || r.Status == "failed") {
		fmt.Fprintln(ctx.Out, "      "+r.Details)
	}
	return r
}

// writeReport persists the result as JSON under
// <PoCDir>/reports/<stamp>/scenarios/<name>.json. If stamp is empty,
// the scenario's started-at time is used; --all passes a shared stamp
// so every scenario in the run lands in one dir alongside run.{json,md}.
// Errors are warned, not raised — a missing report shouldn't fail the run.
func writeReport(pocDir, stamp, name string, r Result, started time.Time) {
	if stamp == "" {
		stamp = started.UTC().Format("2006-01-02T15-04-05Z")
	}
	dir := filepath.Join(pocDir, "reports", stamp, "scenarios")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	full := struct {
		Result
		Scenario  string    `json:"scenario"`
		StartedAt time.Time `json:"started_at"`
		Duration  string    `json:"duration"`
	}{
		Result:    r,
		Scenario:  name,
		StartedAt: started.UTC(),
		Duration:  time.Since(started).Truncate(time.Second).String(),
	}
	data, _ := json.MarshalIndent(full, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644)
}

// RunSummary is the aggregate report `--all` writes at the end of a
// multi-scenario run. One row per scenario; status counts up top so a
// human or CI can read it in 5 seconds.
type RunSummary struct {
	StartedAt time.Time      `json:"started_at"`
	Finished  time.Time      `json:"finished_at"`
	Duration  string         `json:"duration"`
	Total     int            `json:"total"`
	Passed    int            `json:"passed"`
	Failed    int            `json:"failed"`
	Skipped   int            `json:"skipped"`
	Scenarios []SummaryEntry `json:"scenarios"`
}

// SummaryEntry is one row in RunSummary.Scenarios.
type SummaryEntry struct {
	Name    string `json:"name"`
	Rating  string `json:"rating"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// WriteRunSummary persists the aggregate run.json + run.md under
// <PoCDir>/reports/<stamp>/. Best-effort: errors are returned but the
// caller is expected to keep going (we already have per-scenario JSONs).
func WriteRunSummary(pocDir, stamp string, sum RunSummary) error {
	dir := filepath.Join(pocDir, "reports", stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), data, 0o644); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# scenario run %s\n\n", stamp)
	fmt.Fprintf(&b, "- started:  %s\n", sum.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- finished: %s\n", sum.Finished.Format(time.RFC3339))
	fmt.Fprintf(&b, "- duration: %s\n", sum.Duration)
	fmt.Fprintf(&b, "- total: %d   passed: %d   failed: %d   skipped: %d\n\n",
		sum.Total, sum.Passed, sum.Failed, sum.Skipped)
	fmt.Fprintln(&b, "| Scenario | Rating | Status | Summary |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, e := range sum.Scenarios {
		fmt.Fprintf(&b, "| [`%s`](scenarios/%s.json) | %s | %s | %s |\n",
			e.Name, e.Name, e.Rating, e.Status, mdEscape(e.Summary))
	}
	return os.WriteFile(filepath.Join(dir, "run.md"), []byte(b.String()), 0o644)
}

func mdEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// Cleanup runs the scenario's Cleanup hook and emits a one-line
// status to ctx.Out.
func Cleanup(ctx *Context, s Scenario) error {
	fmt.Fprintf(ctx.Out, "scenario:  %s\ncleaning...\n", s.Name())
	if err := s.Cleanup(ctx); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Out, "OK")
	return nil
}

// EnsureScenarioDir creates <PoCDir>/artifacts/scenarios/<name>/ and
// returns its absolute path. Scenarios use this to write rendered
// manifests so the operator can inspect them later.
func EnsureScenarioDir(pocDir, name string) (string, error) {
	dir := filepath.Join(pocDir, "artifacts", "scenarios", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteManifest writes body to <PoCDir>/artifacts/scenarios/<name>/<file>.
// Returns the absolute path written.
func WriteManifest(pocDir, name, file, body string) (string, error) {
	dir, err := EnsureScenarioDir(pocDir, name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Discard is an io.Writer that drops everything — handy for tests
// that don't want scenario chatter on stderr.
var Discard io.Writer = io.Discard
