package cli

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/update"
)

const (
	releaseTimeout = 30 * time.Second
	installTimeout = 5 * time.Minute
)

// Test seams: the binary ccswap update replaces, and GitHub.
var (
	updateTarget   = selfBin
	latestRelease  = update.Latest
	installRelease = update.Install
)

// reinstall runs the new binary's own daemon install, so the service and the hook are written as the new
// version writes them; a test seam.
var reinstall = func(cmd *cobra.Command, bin string) error {
	c := exec.CommandContext(cmd.Context(), bin, "daemon", "install")
	c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
	return c.Run()
}

func newUpdateCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use: "update", Short: i18n.T("update.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, out := cmd.Root().Version, cmd.OutOrStdout()
			ctx, cancel := within(cmd, releaseTimeout)
			rel, err := latestRelease(ctx, "ccswap/"+version)
			cancel()
			switch {
			case err != nil:
				return err
			case !update.Known(version):
				return i18n.Err("update.not_release", version, rel.Version)
			case !update.Newer(rel.Version, version):
				fmt.Fprintf(out, i18n.T("update.latest"), version)
				return nil
			case check:
				fmt.Fprintf(out, i18n.T("update.available"), rel.Version, version, rel.URL)
				return nil
			}
			bin, err := updateTarget()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, i18n.T("update.installing"), version, rel.Version)
			ctx, cancel = within(cmd, installTimeout)
			defer cancel()
			if err := installRelease(ctx, rel, bin); err != nil {
				return err
			}
			fmt.Fprintf(out, i18n.T("update.installed"), bin)
			return restartDaemon(cmd, bin, rel.Version)
		},
	}
	c.Flags().BoolVar(&check, "check", false, i18n.T("update.check_flag"))
	return c
}

// restartDaemon puts the daemon on the new binary: through its service when one is installed, else, for a
// daemon run by hand or by another supervisor, by asking the user.
func restartDaemon(cmd *cobra.Command, bin, version string) error {
	if _, err := platformService().ProgramPath(); err == nil {
		if err := reinstall(cmd, bin); err != nil {
			return i18n.Err("update.reinstall_failed", version, err)
		}
		return nil
	}
	if daemonUp(cmd.Context()) {
		fmt.Fprintf(cmd.OutOrStdout(), i18n.T("update.restart_daemon"), version)
	}
	return nil
}
