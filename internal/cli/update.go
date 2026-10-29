package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/update"
)

const installTimeout = 5 * time.Minute

// restartWait is how long ccswap update waits for the daemon to come back on the new release; a test seam.
var restartWait = time.Minute

// Test seams: the binary ccswap update replaces, and GitHub.
var (
	updateTarget   = selfBin
	latestRelease  = update.Latest
	installRelease = update.Install
)

func newUpdateCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use: "update", Short: i18n.T("update.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, out := cmd.Root().Version, cmd.OutOrStdout()
			if !update.Known(version) && !check { // nothing to ask GitHub: no release can be compared with it
				return i18n.Err("update.not_release_build", version)
			}
			rel, err := latestRelease(cmd.Context(), update.UserAgent(version))
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
			ctx, cancel := within(cmd, installTimeout)
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

// restartDaemon puts a daemon that runs bin on the new release as the page's Update does: it restarts in its
// own process, and its service and the hook stay as they are. A daemon that runs another binary is left alone.
func restartDaemon(cmd *cobra.Command, bin, version string) error {
	out := cmd.OutOrStdout()
	cl, err := client()
	if err != nil {
		return err
	}
	ctx, cancel := within(cmd, readTimeout)
	defer cancel()
	r, err := cl.RestartWithResponse(ctx, api.RestartRequest{Bin: bin})
	switch {
	case errors.Is(down(err), errDown):
		fmt.Fprintf(out, i18n.T("update.next_start"), version)
		return nil
	case err != nil:
		return i18n.Err("update.restart_failed", version, down(err))
	case r.StatusCode() == 404: // a daemon older than ccswap update
		fmt.Fprintf(out, i18n.T("update.restart_daemon"), version)
		return nil
	}
	if err := apiErr(r.StatusCode(), r.Body, "", 200); err != nil {
		return i18n.Err("update.restart_failed", version, err)
	}
	switch {
	case r.JSON200 == nil:
		return errBadResp
	case r.JSON200.Restarting:
		fmt.Fprintf(out, i18n.T("update.restarting"), version)
		return waitDaemon(cmd, cl, version)
	case r.JSON200.Bin == "":
		fmt.Fprintf(out, i18n.T("update.restart_daemon"), version)
	default:
		fmt.Fprintf(out, i18n.T("update.other_daemon"), r.JSON200.Bin)
	}
	return nil
}

// waitDaemon waits for the daemon to answer as version once it restarted.
func waitDaemon(cmd *cobra.Command, cl *api.ClientWithResponses, version string) error {
	ctx, cancel := within(cmd, restartWait)
	defer cancel()
	for {
		if r, err := cl.GetStatusWithResponse(ctx); err == nil && r.JSON200 != nil && r.JSON200.Version == version {
			fmt.Fprintf(cmd.OutOrStdout(), i18n.T("update.daemon_runs"), version)
			return nil
		}
		select {
		case <-ctx.Done():
			return i18n.Err("update.daemon_lost", version)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
