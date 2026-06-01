package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/version"
)

func newDoctorCmd() *cobra.Command {
	var strict bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify host tooling (docker/podman, kind, kubectl, helm) and report resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), strict)
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "Fail on warnings (e.g. min-resources not measured)")
	return cmd
}

type checkResult struct {
	name   string
	status string // ok | warn | fail
	detail string
}

func runDoctor(ctx context.Context, out io.Writer, strict bool) error {
	var results []checkResult

	results = append(results, checkRuntime(ctx))
	// Check whichever cluster backend this invocation drives (kind by
	// default; k3d when invoked via the k3dbnkctl symlink).
	results = append(results, checkBinary(ctx, string(SelectedBackend()), []string{"version"}))
	results = append(results, checkBinary(ctx, "kubectl", []string{"version", "--client=true", "--output=yaml"}))
	results = append(results, checkBinary(ctx, "helm", []string{"version", "--short"}))
	results = append(results, checkResources())

	fails, warns := 0, 0
	for _, r := range results {
		badge := "✓"
		if r.status == "warn" {
			badge = "!"
			warns++
		}
		if r.status == "fail" {
			badge = "✗"
			fails++
		}
		fmt.Fprintf(out, "  [%s] %-12s  %s\n", badge, r.name, r.detail)
	}
	fmt.Fprintln(out)
	if fails > 0 {
		return fmt.Errorf("%d failed check(s) — install the missing tools and retry", fails)
	}
	if strict && warns > 0 {
		return fmt.Errorf("%d warning(s) with --strict", warns)
	}
	fmt.Fprintln(out, "ready.")
	return nil
}

// checkRuntime tries docker then podman.
func checkRuntime(ctx context.Context) checkResult {
	for _, rt := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(rt); err != nil {
			continue
		}
		c := exec.CommandContext(ctx, rt, "version")
		var stderr bytes.Buffer
		c.Stderr = &stderr
		c.Stdout = io.Discard
		if err := c.Run(); err != nil {
			return checkResult{rt, "fail",
				fmt.Sprintf("found on PATH but `%s version` failed: %v", rt, err)}
		}
		return checkResult{rt, "ok", "available"}
	}
	return checkResult{"runtime", "fail", "neither docker nor podman found on PATH"}
}

func checkBinary(ctx context.Context, name string, args []string) checkResult {
	if _, err := exec.LookPath(name); err != nil {
		return checkResult{name, "fail", "not on PATH"}
	}
	c := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return checkResult{name, "fail",
			fmt.Sprintf("`%s %s` failed: %s", name, strings.Join(args, " "),
				strings.TrimSpace(stderr.String()))}
	}
	first := strings.SplitN(strings.TrimSpace(stdout.String()), "\n", 2)[0]
	return checkResult{name, "ok", first}
}

func checkResources() checkResult {
	cores := runtime.NumCPU()
	if !version.Measured() {
		return checkResult{
			"resources", "warn",
			fmt.Sprintf("host: %d cores | min baseline: not yet measured (TBD)", cores),
		}
	}
	if cores < version.MinBaseline.Cores {
		return checkResult{"resources", "fail",
			fmt.Sprintf("host: %d cores  <  min baseline %d cores",
				cores, version.MinBaseline.Cores)}
	}
	return checkResult{"resources", "ok",
		fmt.Sprintf("host: %d cores  ≥  min baseline %d cores",
			cores, version.MinBaseline.Cores)}
}
