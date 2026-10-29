package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/tui"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func optPriority(cmd *cobra.Command) *int {
	if !cmd.Flags().Changed("priority") {
		return nil
	}
	p, _ := cmd.Flags().GetInt("priority")
	return &p
}

func newLoginCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("login.use"), Short: i18n.T("login.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			// the daemon first: a login it cannot import is a wasted code and a deleted login.
			sctx, cancel := within(cmd, readTimeout)
			st, err := cl.GetStatusWithResponse(sctx)
			cancel()
			if err != nil {
				return down(err)
			}
			if err := apiErr(st.StatusCode(), st.Body, "", 200); err != nil {
				return err
			}
			home, err := config.Home()
			if err != nil {
				return err
			}
			cfg, err := config.Load(home)
			if err != nil {
				return err
			}
			target := claudecode.ServiceName(cfg.Claude.ConfigDir)
			kc := newKeychain(home)
			sweepStale(home, target, kc)
			dir, err := os.MkdirTemp(home, "login-*")
			if err != nil {
				return err
			}
			ok := false
			defer func() { cleanupLogin(dir, target, kc, ok) }()
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, i18n.T("login.other_device_hint"))
			bin := os.Getenv("CCSWAP_CLAUDE_BIN")
			if bin == "" {
				bin = "claude"
			}
			p := exec.CommandContext(ctx, bin, "auth", "login")
			p.Cancel = func() error { return p.Process.Signal(syscall.SIGTERM) }
			p.WaitDelay = 5 * time.Second
			p.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") }), "CLAUDE_CONFIG_DIR="+dir)
			p.Stdout, p.Stderr = out, cmd.ErrOrStderr()
			stdin, err := p.StdinPipe()
			if err != nil {
				return err
			}
			go pasteCodes(cmd.InOrStdin(), stdin, out)
			if err := p.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					return &ExitError{Code: max(ee.ExitCode(), 1), Err: fmt.Errorf("%s: %w", i18n.T("login.claude_failed"), err)}
				}
				return fmt.Errorf(i18n.T("login.could_not_start"), bin, err)
			}
			if ctx.Err() != nil {
				return &ExitError{Code: 1, Err: errors.New(i18n.T("login.interrupted"))}
			}
			fmt.Fprintln(out, i18n.T("login.saving"))
			ictx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.ImportAccountWithResponse(ictx, api.ImportRequest{Name: args[0], ConfigDir: dir, Priority: optPriority(cmd)})
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, args[0], 200); err != nil {
				return err
			}
			if r.JSON200 == nil {
				return errBadResp
			}
			ok = true
			fmt.Fprintf(out, i18n.T("account.added"), r.JSON200.Name, r.JSON200.Email)
			return nil
		},
	}
	c.Flags().Int("priority", 0, i18n.T("account.priority_flag"))
	return c
}

// pasteCodes feeds typed lines to `claude auth login`, which wants CODE#STATE: a pasted callback
// URL is turned into that, and each submitted code is acknowledged (the exchange takes a moment).
func pasteCodes(r io.Reader, w io.WriteCloser, out io.Writer) {
	defer w.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := authCode(sc.Text())
		if line != "" {
			fmt.Fprintln(out, i18n.T("login.checking_code"))
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
	}
}

func authCode(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		if q := u.Query(); q.Get("code") != "" && q.Get("state") != "" {
			return q.Get("code") + "#" + q.Get("state")
		}
	}
	return s
}

// cleanupLogin removes a temp login dir; unless the import succeeded (the daemon then deletes them
// itself) it also deletes any keychain item claude created under the dir's hashed service. A dir
// that is the live login's (target) is left alone.
func cleanupLogin(dir, target string, kc keychain.Store, imported bool) {
	svcs, ok := claudecode.LoginServices(dir, target)
	if !ok {
		return
	}
	if !imported {
		for _, s := range svcs {
			_ = kc.Delete(s, claudecode.KeychainAccount())
		}
	}
	_ = os.RemoveAll(dir)
}

// sweepStale cleans login dirs left by a killed ccswap.
func sweepStale(home, target string, kc keychain.Store) {
	ds, _ := filepath.Glob(filepath.Join(home, "login-*"))
	for _, d := range ds {
		if st, err := os.Stat(d); err == nil && time.Since(st.ModTime()) > time.Hour {
			cleanupLogin(d, target, kc, false)
		}
	}
}

func newAdoptCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("adopt.use"), Short: i18n.T("adopt.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.AdoptAccountWithResponse(ctx, api.AdoptRequest{Name: args[0], Priority: optPriority(cmd)})
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, args[0], 200); err != nil {
				return err
			}
			if r.JSON200 == nil {
				return errBadResp
			}
			fmt.Fprintf(cmd.OutOrStdout(), i18n.T("account.added"), r.JSON200.Name, r.JSON200.Email)
			return nil
		},
	}
	c.Flags().Int("priority", 0, i18n.T("account.priority_flag"))
	return c
}

func confirm(cmd *cobra.Command, q string) (bool, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
			return false, errors.New(i18n.T("cli.confirm_not_interactive"))
		}
	}
	fmt.Fprint(cmd.ErrOrStderr(), q+" [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes" || a == "д" || a == "да", nil
}

func newRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("remove.use"), Short: i18n.T("remove.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if y, _ := cmd.Flags().GetBool("yes"); !y {
				ok, err := confirm(cmd, i18n.T("remove.confirm", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), i18n.T("remove.cancelled"))
					return nil
				}
			}
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.RemoveAccountWithResponse(ctx, args[0])
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, args[0], 204); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), i18n.T("remove.done"), args[0])
			return nil
		},
	}
	c.Flags().Bool("yes", false, i18n.T("remove.yes_flag"))
	return c
}

func newUseCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("use.use"), Short: i18n.T("use.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			force, _ := cmd.Flags().GetBool("force")
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.ActivateAccountWithResponse(ctx, args[0], &api.ActivateAccountParams{Force: &force})
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, args[0], 200); err != nil {
				if c := parseErr(r.Body).Code; !force && (c == "near_refresh" || c == "cannot_identify") {
					return fmt.Errorf(i18n.T("use.force_hint"), err, args[0])
				}
				return err
			}
			if r.JSON200 == nil {
				return errBadResp
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, i18n.T("use.done"), r.JSON200.Active)
			if w := r.JSON200.Warning; w != nil && *w != "" {
				fmt.Fprintln(out, i18n.T("use.warning"), *w)
			}
			return nil
		},
	}
	c.Flags().Bool("force", false, i18n.T("use.force_flag"))
	return c
}

func pct(w *api.Window) string {
	if w == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", w.Utilization)
}

func stateText(a api.Account) string {
	var s []string
	if a.Exhausted {
		s = append(s, i18n.T("list.limit"))
	}
	if a.NeedsLogin {
		s = append(s, i18n.T("list.needs_login"))
	}
	if !a.Enabled {
		s = append(s, i18n.T("list.off"))
	}
	return strings.Join(s, ", ")
}

func newListCmd() *cobra.Command {
	c := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: i18n.T("list.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, readTimeout)
			defer cancel()
			r, err := cl.ListAccountsWithResponse(ctx)
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, "", 200); err != nil {
				return err
			}
			if r.JSON200 == nil {
				return errBadResp
			}
			out := cmd.OutOrStdout()
			if j, _ := cmd.Flags().GetBool("json"); j {
				_, err := out.Write(r.Body)
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, i18n.T("list.header"))
			for _, a := range *r.JSON200 {
				mark := ""
				if a.Active {
					mark = "▶"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", mark, a.Name, a.Email, a.Priority,
					pct(a.FiveHour), pct(a.SevenDay), tui.ResetText(a, time.Now()), stateText(a))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, i18n.N("list.accounts", len(*r.JSON200)))
			return err
		},
	}
	c.Flags().Bool("json", false, i18n.T("list.json_flag"))
	return c
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: i18n.T("status.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, readTimeout)
			defer cancel()
			r, err := cl.GetStatusWithResponse(ctx)
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, "", 200); err != nil {
				return err
			}
			if r.JSON200 == nil {
				return errBadResp
			}
			s, out := r.JSON200, cmd.OutOrStdout()
			active := s.Active
			if active == "" {
				active = "-"
			}
			auto := i18n.T("status.off")
			if s.Auto {
				auto = i18n.T("status.on")
			}
			poll := i18n.T("status.no_poll_yet")
			if s.LastPollAt != nil {
				poll = s.LastPollAt.Local().Format("15:04:05")
			}
			target := i18n.T("status.keychain", s.TargetService)
			if claudecode.GOOS == "linux" { // the service name is the credentials file there
				target = i18n.T("status.cred_file", s.TargetService)
			}
			fmt.Fprintf(out, i18n.T("status.summary"),
				active, auto, s.Thresholds.FiveHour, s.Thresholds.SevenDay, target, poll)
			if s.UnknownActive {
				fmt.Fprintln(out, i18n.T("status.unknown_active"))
			}
			if s.Update != nil {
				fmt.Fprintf(out, i18n.T("status.update"), s.Update.Version, s.Version)
			}
			return nil
		},
	}
}

func patch(cmd *cobra.Command, name string, p api.AccountPatch, done string) error {
	cl, err := client()
	if err != nil {
		return err
	}
	ctx, cancel := within(cmd, writeTimeout)
	defer cancel()
	r, err := cl.PatchAccountWithResponse(ctx, name, p)
	if err != nil {
		return down(err)
	}
	if err := apiErr(r.StatusCode(), r.Body, name, 200); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), i18n.T("account.changed"), name, done)
	return nil
}

func newPriorityCmd() *cobra.Command {
	return &cobra.Command{
		Use: i18n.T("priority.use"), Short: i18n.T("priority.short"), Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf(i18n.T("priority.not_number"), args[1])
			}
			return patch(cmd, args[0], api.AccountPatch{Priority: &n}, i18n.T("priority.done", args[1]))
		},
	}
}

func newEnableCmd(on bool) *cobra.Command {
	use, done := i18n.T("enable.disable_use"), i18n.T("enable.disabled")
	short := i18n.T("enable.disable_short")
	if on {
		use, done, short = i18n.T("enable.use"), i18n.T("enable.enabled"), i18n.T("enable.short")
	}
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return patch(cmd, args[0], api.AccountPatch{Enabled: &on}, done)
		},
	}
}

func newAutoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "auto on|off", Short: i18n.T("auto.short"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var on bool
			switch args[0] {
			case "on":
				on = true
			case "off":
			default:
				return errors.New(i18n.T("auto.expected_on_off"))
			}
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.SetAutoWithResponse(ctx, api.AutoRequest{Enabled: on})
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, "", 204); err != nil {
				return err
			}
			if on {
				fmt.Fprintln(cmd.OutOrStdout(), i18n.T("auto.on"))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), i18n.T("auto.off"))
			}
			return nil
		},
	}
}
