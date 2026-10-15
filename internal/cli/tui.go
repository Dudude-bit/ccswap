package cli

import (
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/tui"
)

func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use: "tui", Short: i18n.T("tui.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			_, err = tea.NewProgram(tui.New(tui.NewClient(cl), self)).Run()
			return err
		},
	}
}
