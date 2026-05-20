package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/poc"
)

func newValidateCmd() *cobra.Command {
	var (
		pocDir string
		strict bool
	)
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Sanity-check poc.yaml + referenced keys/ files",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolvePoCDir(pocDir)
			if err != nil {
				return err
			}
			p, err := poc.Load(repo)
			if err != nil {
				return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
			}
			r := p.Validate()
			// Extra repo-level checks: keys actually present.
			for _, ref := range []struct {
				label string
				path  string
			}{
				{"bnk.far_key_ref", p.BNK.FARKeyRef},
				{"bnk.jwt_ref", p.BNK.JWTRef},
			} {
				if ref.path == "" {
					continue
				}
				if _, err := os.Stat(resolveRef(repo, ref.path)); err != nil {
					r.Errors = append(r.Errors,
						fmt.Sprintf("%s file %s not found — drop it there and retry",
							ref.label, ref.path))
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "PoC:  %s   (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
			fmt.Fprintf(out, "Repo: %s\n\n", repo)
			if len(r.Errors) == 0 && len(r.Warnings) == 0 {
				fmt.Fprintln(out, "OK — poc.yaml is valid.")
				return nil
			}
			for _, w := range r.Warnings {
				fmt.Fprintln(out, "WARN ", w)
			}
			for _, e := range r.Errors {
				fmt.Fprintln(out, "ERR  ", e)
			}
			if len(r.Errors) > 0 {
				return fmt.Errorf("%d validation error(s)", len(r.Errors))
			}
			if strict && len(r.Warnings) > 0 {
				return fmt.Errorf("%d warning(s) with --strict", len(r.Warnings))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&strict, "strict", false, "Fail on warnings (for CI / unattended use)")
	return cmd
}
