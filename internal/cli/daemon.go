package cli

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/anthropic"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/daemon"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/launchd"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
	"github.com/Dudude-bit/ccswap/internal/update"
)

// newDaemonCmd returns `daemon`; install/uninstall/status are attached from another file.
func newDaemonCmd() *cobra.Command {
	d := &cobra.Command{Use: "daemon", Short: i18n.T("daemon.short")}
	d.AddCommand(&cobra.Command{
		Use: "run", Short: i18n.T("daemon.run_short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := config.Home()
			if err != nil {
				return cannotStart(err)
			}
			sock, err := config.SocketPath(home)
			if err != nil {
				return cannotStart(err)
			}
			cfg, err := config.Load(home)
			if err != nil {
				return cannotStart(err)
			}
			userHome, err := os.UserHomeDir()
			if err != nil {
				return cannotStart(err)
			}
			version := cmd.Root().Version
			bin, _ := selfBin() // without it an update fails and says why; the daemon still runs
			kc := newKeychain(home)
			st, err := store.Open(home, kc)
			if err != nil {
				return cannotStart(err)
			}
			client := anthropic.New()
			client.UserAgent = update.UserAgent(version)
			sw := switcher.New(switcher.Deps{
				KC: kc, Store: st, Profiles: client,
				Service:    claudecode.ServiceName(cfg.Claude.ConfigDir),
				KCAccount:  claudecode.KeychainAccount(),
				ClaudeJSON: claudecode.GlobalConfigPath(userHome, cfg.Claude.ConfigDir),
				ClaudeDir:  claudecode.ConfigDir(userHome, cfg.Claude.ConfigDir),
			})
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			slog.Info("ccswap daemon starting", "version", version, "home", home)
			if bin != "" {
				refreshService(bin)
			}
			err = daemon.New(daemon.Options{
				Config: cfg, Store: st, KC: kc, API: client, Switcher: sw, Notifier: platformNotifier(),
				Home: home, Socket: sock, Version: version, Host: hostname(), Bin: bin,
				Projects: claudecode.ProjectsDir(userHome, cfg.Claude.ConfigDir),
			}).Run(ctx)
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				return &ExitError{Code: noRestartExit(daemon.ExitAlreadyRunning), Err: err}
			}
			if errors.Is(err, daemon.ErrRestart) {
				slog.Info("ccswap daemon restarting on the updated binary", "bin", bin)
				// exec keeps the process launchd, systemd or a terminal started; failing, it exits 1 and is restarted
				return execSelf(bin, os.Args, os.Environ())
			}
			return err
		},
	})
	d.AddCommand(newInstallCmd(), newUninstallCmd(), newDaemonStatusCmd())
	return d
}

// refreshService brings the service's definition up to date when it runs bin, so a release that changed
// what ccswap daemon install writes needs no reinstall after an update; it never restarts the daemon, and
// takes effect at the service's next start.
func refreshService(bin string) {
	svc := platformService()
	switch ok, err := svc.Refresh(bin); {
	case err != nil:
		slog.Warn("service: cannot bring its definition up to date", "service", svc.Label, "err", err)
	case ok:
		slog.Info("service: definition brought up to date, in effect from its next start", "service", svc.Label)
	}
}

// execSelf is syscall.Exec; a test seam.
var execSelf = syscall.Exec

// cannotStart stops a daemon whose files or folders are unusable: every start would fail the same way
// and add to daemon.log.
func cannotStart(err error) error {
	return &ExitError{Code: noRestartExit(daemon.ExitConfig), Err: i18n.Err("daemon.no_restart", err)}
}

// noRestartExit is the exit status for a failure a restart cannot cure: the systemd unit does not
// restart on code, and launchd restarts on anything but 0, so its job gets 0.
func noRestartExit(code int) int {
	if os.Getenv("XPC_SERVICE_NAME") == launchd.Label { // launchd names its job here
		return 0
	}
	return code
}

// hostname is this computer's short name, without a .local or domain suffix.
func hostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return h
}
