package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mwiget/kindbnkctl/internal/bnkforge"
	"github.com/mwiget/kindbnkctl/internal/poc"
)

func newBNKForgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bnk-forge",
		Short: "Manually drive the bnk-forge integration (launch / unregister)",
	}
	cmd.AddCommand(newBNKForgeLaunchCmd())
	cmd.AddCommand(newBNKForgeUnregisterCmd())
	return cmd
}

func newBNKForgeLaunchCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Ensure bnk-forge sees this PoC's cluster (create project + register kubeconfig)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolvePoCDir(pocDir)
			if err != nil {
				return err
			}
			p, err := poc.Load(repo)
			if err != nil {
				return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
			}
			if !p.BNKForge.Enabled {
				return errors.New("bnk_forge.enabled is false in poc.yaml — flip it true and retry")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "PoC: %s\n\n", p.Metadata.Name)
			if err := registerWithBNKForge(cmd.Context(), out, repo, p); err != nil {
				if errors.Is(err, bnkforge.ErrNotRunning) {
					return err
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func newBNKForgeUnregisterCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "unregister",
		Short: "Remove this PoC's cluster + project from bnk-forge",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolvePoCDir(pocDir)
			if err != nil {
				return err
			}
			p, err := poc.Load(repo)
			if err != nil {
				return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
			}
			if !p.BNKForge.Enabled {
				return errors.New("bnk_forge.enabled is false in poc.yaml — nothing to unregister")
			}
			return unregisterFromBNKForge(cmd.Context(), cmd.OutOrStdout(), p)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}
