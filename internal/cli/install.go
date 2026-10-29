package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/hook"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/systemd"
)

// keychainHas reports whether a keychain item exists for service (never reads the secret). Test seam.
var keychainHas = func(service string) bool {
	if claudecode.GOOS == "linux" { // service is the credentials file
		_, err := os.Lstat(service)
		return err == nil
	}
	return exec.Command("/usr/bin/security", "find-generic-password", "-s", service).Run() == nil
}

type setup struct {
	home, userHome, settings string
	cfg                      config.Config
}

func loadSetup() (setup, error) {
	var s setup
	var err error
	if s.home, err = config.Home(); err != nil {
		return s, err
	}
	if s.cfg, err = config.Load(s.home); err != nil {
		return s, i18n.Err("daemon.cannot_start", err)
	}
	if s.userHome, err = os.UserHomeDir(); err != nil {
		return s, err
	}
	s.settings = claudecode.SettingsPath(s.userHome, s.cfg.Claude.ConfigDir)
	return s, nil
}

func selfBin() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use: "install", Short: i18n.T("daemon.install_short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSetup()
			if err != nil {
				return err
			}
			if err := startable(s.home); err != nil { // the daemon would stop at once
				return err
			}
			bin, err := selfBin()
			if err != nil {
				return err
			}
			// the service gets no shell PATH (launchd's is /usr/bin:/bin): it keeps the claude this shell runs
			if os.Getenv("CCSWAP_CLAUDE_BIN") == "" {
				if p, err := exec.LookPath("claude"); err == nil && filepath.IsAbs(p) {
					os.Setenv("CCSWAP_CLAUDE_BIN", p)
					defer os.Unsetenv("CCSWAP_CLAUDE_BIN")
				}
			}
			out := cmd.OutOrStdout()
			svc := platformService()
			var le *systemd.LingerError
			if err := svc.Install(bin, s.home); errors.As(err, &le) {
				defer fmt.Fprintln(out, le.Error()) // not fatal: the hook below still goes in
			} else if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s %s %s\n", svc.Name, svc.Label, svc.Installed)
			if err := hook.Install(s.settings, hook.Command(bin)); err != nil {
				return fmt.Errorf("%s: %w", i18n.T("daemon.hook_not_installed"), err)
			}
			fmt.Fprintf(out, i18n.T("daemon.hook_added"), s.settings)
			return nil
		},
	}
}

func newUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use: "uninstall", Short: i18n.T("daemon.uninstall_short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := config.Home()
			if err != nil {
				return err
			}
			userHome, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			// the service needs no config, and only [claude] config_dir moves the hook; state.json is not read
			cfg, err := config.Load(home)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), i18n.T("daemon.uninstall_bad_config", err))
				cfg = config.Config{}
			}
			settings := claudecode.SettingsPath(userHome, cfg.Claude.ConfigDir)
			bin, err := selfBin()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if err := hook.Remove(settings, hook.Command(bin)); err != nil {
				return fmt.Errorf("%s: %w", i18n.T("daemon.hook_not_removed"), err)
			}
			fmt.Fprintf(out, i18n.T("daemon.hook_removed"), settings)
			svc := platformService()
			if err := svc.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s %s\n", svc.Name, svc.Removed)
			fmt.Fprintln(out, i18n.T("daemon.uninstall_kept"))
			return nil
		},
	}
}

// startable reports what in home would stop the daemon at once, past what loadSetup loads: a socket
// path too long or a state.json that does not load.
func startable(home string) error {
	if _, err := config.SocketPath(home); err != nil {
		return err
	}
	_, err := store.Open(home, nil)
	return err
}

// daemonUp asks the daemon for its status over the socket.
func daemonUp(ctx context.Context) bool {
	cl, err := client()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := cl.GetStatusWithResponse(ctx)
	return err == nil && r.StatusCode() == 200
}

type check struct {
	ok  bool
	msg string
}

func (c check) line() string {
	if c.ok {
		return "✓ " + c.msg
	}
	return "✗ " + c.msg
}

func installChecks(ctx context.Context, s setup) []check {
	var cs []check
	svc := platformService()
	loaded, lerr := svc.Loaded()
	noSystemd := errors.Is(lerr, systemd.ErrNoSystemd)
	if noSystemd {
		cs = append(cs, check{false, lerr.Error()})
	} else {
		cs = append(cs, check{loaded, map[bool]string{true: svc.Name + " " + svc.LoadedText, false: svc.Name + " " + svc.NotLoadedText + " (ccswap daemon install)"}[loaded]})
	}
	up := daemonUp(ctx)
	cs = append(cs, check{up, map[bool]string{true: i18n.T("doctor.daemon_up"), false: i18n.T("doctor.daemon_down")}[up]})
	if !up {
		if err := startable(s.home); err != nil {
			cs = append(cs, check{false, i18n.T("daemon.cannot_start", err)})
		}
	}
	bin, berr := selfBin()
	exact, stale := false, false
	if berr == nil {
		exact, stale, _ = hook.Installed(s.settings, hook.Command(bin))
	}
	switch {
	case stale:
		cs = append(cs, check{false, i18n.T("doctor.hook_old_path")})
	case exact:
		cs = append(cs, check{true, i18n.T("doctor.hook_installed")})
	default:
		cs = append(cs, check{false, i18n.T("doctor.hook_not_installed")})
	}
	if p, err := svc.ProgramPath(); err == nil && berr == nil && p != bin {
		cs = append(cs, check{false, i18n.T("doctor.service_runs_other", svc.Name, p, bin)})
	}
	if claudecode.GOOS == "linux" && !noSystemd {
		if on, _ := systemd.Linger(); on {
			cs = append(cs, check{true, i18n.T("doctor.linger_on")})
		} else {
			cs = append(cs, check{false, i18n.T("doctor.linger_off", systemd.User())})
		}
	}
	return cs
}

func hasText(has bool) string {
	if has {
		return i18n.T("doctor.present")
	}
	return i18n.T("doctor.missing")
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: i18n.T("daemon.status_short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSetup()
			if err != nil {
				return err
			}
			for _, c := range installChecks(cmd.Context(), s) {
				fmt.Fprintln(cmd.OutOrStdout(), c.line())
			}
			return nil
		},
	}
}

// getuid is a test seam.
var getuid = os.Getuid

// credFileChecks warns about a symlinked, loose or foreign credentials file (stat only, never read).
func credFileChecks(path string) []check {
	var cs []check
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		cs = append(cs, check{false, i18n.T("doctor.cred_file_symlink", path)})
	}
	fi, err := os.Stat(path)
	if err != nil {
		return cs
	}
	if fi.Mode().Perm() != 0o600 {
		cs = append(cs, check{false, i18n.T("doctor.cred_file_mode", fi.Mode().Perm())})
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		cs = append(cs, check{false, i18n.T("doctor.cred_file_owner", st.Uid)})
	}
	return cs
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: i18n.T("doctor.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSetup()
			if err != nil {
				return err
			}
			var cs []check
			_, lerr := exec.LookPath("claude")
			cs = append(cs, check{lerr == nil, map[bool]string{true: i18n.T("doctor.claude_found"), false: i18n.T("doctor.claude_not_found")}[lerr == nil]})
			svc := claudecode.ServiceName(s.cfg.Claude.ConfigDir)
			has := keychainHas(svc)
			if claudecode.GOOS == "linux" {
				cs = append(cs, check{has, i18n.T("doctor.cred_file", svc, hasText(has))})
				cs = append(cs, credFileChecks(svc)...)
			} else {
				cs = append(cs, check{has, i18n.T("doctor.keychain_entry", svc, hasText(has))})
			}
			if os.Getenv("CLAUDE_SECURESTORAGE_CONFIG_DIR") != "" {
				cs = append(cs, check{false, i18n.T("doctor.securestorage_set")})
			}
			if envSvc := claudecode.ServiceName(os.Getenv("CLAUDE_CONFIG_DIR")); envSvc != svc {
				what := i18n.T("doctor.the_entry")
				if claudecode.GOOS == "linux" {
					what = i18n.T("doctor.the_cred_file")
				}
				cs = append(cs, check{false, i18n.T("doctor.config_dir_mismatch", os.Getenv("CLAUDE_CONFIG_DIR"), what, envSvc, svc, s.cfg.Claude.ConfigDir)})
			}
			cs = append(cs, installChecks(cmd.Context(), s)...)
			if s.cfg.T3.Enabled {
				conn, err := net.DialTimeout("tcp", s.cfg.T3.Listen, 2*time.Second)
				if err == nil {
					conn.Close()
				}
				cs = append(cs, check{err == nil, map[bool]string{true: i18n.T("doctor.t3_up", s.cfg.T3.Listen), false: i18n.T("doctor.t3_down", s.cfg.T3.Listen)}[err == nil]})
			}
			bad := false
			for _, c := range cs {
				fmt.Fprintln(cmd.OutOrStdout(), c.line())
				bad = bad || !c.ok
			}
			if bad {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
}
