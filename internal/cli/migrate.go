package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/tui"
)

var (
	errNoTTY       error = i18n.Err("migrate.no_tty")
	errInterrupted error = i18n.Err("migrate.interrupted")
	errTooLarge    error = i18n.Err("migrate.too_large")
)

// bundleLimit caps a bundle read: a real one is a few KiB.
const bundleLimit = 1 << 20

// answerTimeout is how long a commit, cancel or import waits for the daemon; a var for tests.
var answerTimeout = writeTimeout

// migrateFrozen is how long an export the CLI did not hear of may keep its accounts frozen: the daemon's 5
// minutes, and one for its answer.
const migrateFrozen = 6 * time.Minute

// askCode reads the code from the terminal, unechoed: stdin may carry the bundle. A seam for tests.
var askCode = func(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errNoTTY
	}
	st, err := term.GetState(tty.Fd())
	if err != nil {
		tty.Close()
		return "", errNoTTY
	}
	// Go's default exit on a signal would skip ReadPassword's restore, leaving echo off, and drop a
	// bundle the caller still has to save: a signal restores the terminal and returns instead.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sig)
	type read struct {
		b   []byte
		err error
	}
	got := make(chan read, 1)
	fmt.Fprint(tty, prompt)
	go func() {
		b, err := term.ReadPassword(tty.Fd())
		runtime.KeepAlive(tty) // its finalizer must not close the descriptor under the read
		got <- read{b, err}
	}()
	select {
	case r := <-got:
		term.Restore(tty.Fd(), st)
		fmt.Fprintln(tty)
		tty.Close()
		if r.err != nil {
			return "", errInterrupted // Ctrl-D: a bare EOF
		}
		return strings.TrimSpace(string(r.b)), nil
	case <-sig:
		term.Restore(tty.Fd(), st)
		fmt.Fprintln(tty)
		return "", errInterrupted // tty stays open: the read still uses its descriptor
	}
}

// holdSignals catches the signals that would stop ccswap and drops them until stop is called: in a critical
// section a bundle may be the only copy of a login. With SIGPIPE caught a write to a closed pipe fails with
// EPIPE instead of killing ccswap.
func holdSignals() (stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE)
	return func() { signal.Stop(sig) }
}

// isTerminal reports whether v is a file that is a terminal; a seam for tests.
var isTerminal = func(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func newMigrateCmd() *cobra.Command {
	c := &cobra.Command{Use: "migrate", Short: i18n.T("migrate.short"), Long: i18n.T("migrate.long")}
	c.AddCommand(newMigrateExportCmd(), newMigrateImportCmd())
	return c
}

func newMigrateExportCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("migrate.export_use"), Short: i18n.T("migrate.export_short"), Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var names []string // the daemon packs a name once
			for _, n := range args {
				if !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
			file, _ := cmd.Flags().GetString("output")
			if file != "" {
				if _, err := os.Lstat(file); err == nil {
					return errors.New(i18n.T("migrate.file_exists", file))
				}
			}
			errw := cmd.ErrOrStderr()
			if file == "" && !isTerminal(cmd.OutOrStdout()) {
				fmt.Fprintln(errw, i18n.T("migrate.pipe_warning"))
			}
			if y, _ := cmd.Flags().GetBool("yes"); !y {
				ok, err := confirm(cmd, i18n.T("migrate.confirm", strings.Join(names, ", ")))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(errw, i18n.T("migrate.cancelled_by_user"))
					return nil
				}
			}
			cl, err := client()
			if err != nil {
				return err
			}
			// Nothing may stop an export between packing and a settled outcome with the bundle safe.
			defer holdSignals()()
			ctx, cancel := within(cmd, writeTimeout)
			r, err := cl.MigrateExportWithResponse(ctx, api.MigrateExportRequest{Names: names})
			cancel()
			if tui.Timeout(err) { // the daemon may have packed and frozen them, with no token to cancel
				return i18n.Err("migrate.export_slow", time.Now().Add(migrateFrozen).Format("15:04"))
			}
			if err != nil {
				return down(err)
			}
			if r.StatusCode() != 200 {
				return migrateErr(r.StatusCode(), r.Body)
			}
			x := r.JSON200
			if x == nil || x.Token == "" || x.Bundle == "" || x.Code == "" {
				if x != nil && x.Token != "" {
					cancelExport(cmd.Context(), cl, x.Token)
				}
				return errBadResp
			}
			if file != "" {
				return exportToFile(cmd, cl, x, names, file)
			}
			return exportToPipe(cmd, cl, x, names)
		},
	}
	c.Flags().StringP("output", "o", "", i18n.T("migrate.output_flag"))
	c.Flags().Bool("yes", false, i18n.T("migrate.yes_flag"))
	return c
}

// exportToFile writes the bundle first and commits after: a failed commit leaves the accounts here.
func exportToFile(cmd *cobra.Command, cl *api.ClientWithResponses, x *api.MigrateExport, names []string, file string) error {
	errw := cmd.ErrOrStderr()
	if err := writeNew(file, []byte(x.Bundle)); err != nil {
		cancelExport(cmd.Context(), cl, x.Token)
		fmt.Fprintln(errw, i18n.T("migrate.write_failed", err))
		fmt.Fprintln(errw, i18n.T("migrate.export_cancelled"))
		return &ExitError{Code: 1}
	}
	switch settle(cmd.Context(), errw, cl, x.Token, names) {
	case committed:
		fmt.Fprintln(errw, i18n.T("migrate.exported_file", strings.Join(names, ", "), file))
		fmt.Fprintln(errw, i18n.T("migrate.code", x.Code))
		fmt.Fprintln(errw, i18n.T("migrate.import_once"))
		return nil
	case notCommitted:
		if err := os.Remove(file); err != nil {
			fmt.Fprintln(errw, i18n.T("migrate.remove_failed", err))
		}
		fmt.Fprintln(errw, i18n.T("migrate.export_cancelled"))
	default:
		fmt.Fprintln(errw, i18n.T("migrate.unknown_file", file, settledBy(x), strings.Join(names, ", ")))
		fmt.Fprintln(errw, i18n.T("migrate.code", x.Code))
		fmt.Fprintln(errw, i18n.T("migrate.import_once"))
	}
	return &ExitError{Code: 1}
}

// exportToPipe commits first and writes after: a pipe cannot tell whether the bundle arrived, and a
// lost login beats two copies of it. A bundle stdout does not take is saved.
func exportToPipe(cmd *cobra.Command, cl *api.ClientWithResponses, x *api.MigrateExport, names []string) error {
	errw := cmd.ErrOrStderr()
	switch settle(cmd.Context(), errw, cl, x.Token, names) {
	case notCommitted:
		fmt.Fprintln(errw, i18n.T("migrate.export_cancelled"))
		return &ExitError{Code: 1}
	case committed:
		if _, err := io.WriteString(cmd.OutOrStdout(), x.Bundle); err == nil {
			fmt.Fprintln(errw, i18n.T("migrate.code", x.Code))
			fmt.Fprintln(errw, i18n.T("migrate.import_once"))
			return nil
		}
		return keepBundle(errw, x, names, func(p string) string { return i18n.T("migrate.pipe_failed_saved", p) })
	}
	return keepBundle(errw, x, names, func(p string) string {
		return i18n.T("migrate.unknown_saved", p, settledBy(x), strings.Join(names, ", "))
	})
}

// settledBy is when ccswap list tells whether an export moved: its commit lands before the deadline or
// never; the minute covers a commit that took the daemon's lock just before it.
func settledBy(x *api.MigrateExport) string {
	return x.ExpiresAt.Add(time.Minute).Local().Format("15:04:05")
}

// keepBundle saves a bundle that has no other home, then says where (msg) with the code; one it cannot save
// is shown on stderr instead, never stdout: stdout is the pipe.
func keepBundle(errw io.Writer, x *api.MigrateExport, names []string, msg func(path string) string) error {
	if p, err := saveBundle("export", []byte(x.Bundle)); err != nil {
		fmt.Fprintln(errw, i18n.T("migrate.not_saved", err, settledBy(x), strings.Join(names, ", ")))
		fmt.Fprintln(errw, strings.TrimSuffix(x.Bundle, "\n"))
	} else {
		fmt.Fprintln(errw, msg(p))
	}
	fmt.Fprintln(errw, i18n.T("migrate.code", x.Code))
	fmt.Fprintln(errw, i18n.T("migrate.import_once"))
	return &ExitError{Code: 1}
}

type outcome int

const (
	committed outcome = iota
	notCommitted
	unknown
)

// settle commits an export and learns whether the commit landed. A commit without an answer may have
// landed or not: cancel tells (export_done is a landed one); a daemon down or restarted leaves it to
// state.json.
func settle(ctx context.Context, errw io.Writer, cl *api.ClientWithResponses, token string, names []string) outcome {
	cctx, cancel := context.WithTimeout(ctx, answerTimeout)
	r, err := cl.MigrateCommitWithResponse(cctx, api.MigrateTokenRequest{Token: token})
	cancel()
	if err == nil {
		if r.StatusCode() == 204 {
			return committed
		}
		fmt.Fprintln(errw, migrateErr(r.StatusCode(), r.Body))
		return notCommitted
	}
	fmt.Fprintln(errw, i18n.T("migrate.settling")) // a silent wait that ignores Ctrl-C invites kill -9
	for range 3 {
		cctx, cancel := context.WithTimeout(ctx, answerTimeout)
		r, err := cl.MigrateCancelWithResponse(cctx, api.MigrateTokenRequest{Token: token})
		cancel()
		switch {
		case err != nil && errors.Is(down(err), errDown):
			return fromState(names)
		case err != nil: // slow or broken connection: ask again
		case r.StatusCode() == 204:
			return notCommitted
		case parseErr(r.Body).Code == "export_done":
			return committed
		case parseErr(r.Body).Code == "export_gone":
			return fromState(names)
		}
		// any other answer asks again too
	}
	return unknown
}

// fromState judges a commit by state.json: a commit drops the accounts from it, a restart keeps them.
func fromState(names []string) outcome {
	home, err := config.Home()
	if err != nil {
		return unknown
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); err != nil {
		return unknown // Open would read a missing state.json as one without accounts
	}
	s, err := store.Open(home, nil)
	if err != nil {
		return unknown
	}
	st, here := s.State(), 0
	for _, n := range names {
		if _, ok := st.Find(n); ok {
			here++
		}
	}
	switch here {
	case 0:
		return committed
	case len(names):
		return notCommitted
	}
	return unknown
}

// cancelExport drops an export, best effort: one never committed expires on its own.
func cancelExport(ctx context.Context, cl *api.ClientWithResponses, token string) {
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	_, _ = cl.MigrateCancelWithResponse(ctx, api.MigrateTokenRequest{Token: token})
}

// saveBundle keeps b in a new file under <home>/migrate and returns its path.
func saveBundle(prefix string, b []byte) (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "migrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Join(dir, prefix+"-"+time.Now().Format("20060102-150405"))
	for i := 1; ; i++ {
		p := base + ".ccswap"
		if i > 1 {
			p = fmt.Sprintf("%s-%d.ccswap", base, i)
		}
		if err := writeNew(p, b); !errors.Is(err, fs.ErrExist) {
			if err != nil {
				return "", err
			}
			return p, nil
		}
	}
}

// writeNew writes b durably to a file it creates: it never replaces one and leaves none half written.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	atomicfile.SyncDir(filepath.Dir(path))
	return nil
}

func newMigrateImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use: i18n.T("migrate.import_use"), Short: i18n.T("migrate.import_short"), Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := readBundle(cmd, args)
			if err != nil {
				return err
			}
			file := ""
			if len(args) == 1 {
				file = args[0]
			}
			// From here a bundle from stdin may be the only copy of the login (a pipe's export removed the
			// accounts already), and an import sent may land: nothing may stop ccswap before it says which.
			defer holdSignals()()
			switch err = migrate.Check(b); {
			case errors.Is(err, migrate.ErrNewerVersion):
				err = i18n.Err("daemon.bundle_version") // kept: an updated ccswap imports it
			case err != nil:
				return i18n.Err("daemon.not_bundle")
			default:
				err = importBundle(cmd, b, file)
			}
			if err == nil {
				return nil
			}
			_, unknown := errors.AsType[unknownErr](err)
			if file != "" && !unknown {
				return err // the file stays where it is
			}
			errw := cmd.ErrOrStderr()
			if !unknown {
				fmt.Fprintln(errw, err)
			}
			p := file
			if file == "" {
				var serr error
				if p, serr = saveBundle("received", b); serr != nil {
					if unknown {
						fmt.Fprintln(errw, i18n.T("migrate.import_unknown_shown"))
					}
					fmt.Fprintln(errw, i18n.T("migrate.save_failed", serr))
					cmd.OutOrStdout().Write(b)
					return &ExitError{Code: 1}
				}
			}
			if retry := "ccswap migrate import " + shellQuote(p); unknown {
				fmt.Fprintln(errw, i18n.T("migrate.import_unknown", p, retry))
			} else {
				fmt.Fprintln(errw, i18n.T("migrate.bundle_saved", p, retry))
			}
			return &ExitError{Code: 1}
		},
	}
	c.Flags().String("code", "", i18n.T("migrate.code_flag"))
	c.Flags().Bool("replace", false, i18n.T("migrate.replace_flag"))
	return c
}

// readBundle reads the bundle from the file in args, else from stdin, a paste at a terminal: its lines from
// BEGIN to END, trimmed and without blank ones. Text around them (a shell banner on a pipe, a stray line in
// a paste) is not the bundle's, and the read stops at END: a terminal sends no EOF after a paste.
func readBundle(cmd *cobra.Command, args []string) ([]byte, error) {
	in := cmd.InOrStdin()
	paste := false
	if len(args) == 1 {
		f, err := os.Open(args[0])
		if err != nil {
			return nil, i18n.Err("migrate.read_failed", err)
		}
		defer f.Close()
		in = f
	} else if paste = isTerminal(in); paste {
		fmt.Fprintln(cmd.ErrOrStderr(), i18n.T("migrate.paste"))
	}
	r := bufio.NewReader(io.LimitReader(in, bundleLimit+1))
	var b bytes.Buffer
	for n := 0; ; {
		line, err := r.ReadString('\n')
		if n += len(line); n > bundleLimit {
			return nil, errTooLarge
		}
		l := strings.TrimSpace(line)
		if i := strings.Index(l, migrate.BeginPrefix); b.Len() == 0 && i >= 0 {
			l = l[i:] // a banner with no newline of its own may come first
		}
		if l != "" && (b.Len() > 0 || strings.HasPrefix(l, migrate.BeginPrefix)) {
			end := strings.Index(l, migrate.EndLine)
			if end >= 0 {
				l = l[:end+len(migrate.EndLine)]
			}
			b.WriteString(l + "\n")
			if end >= 0 {
				if f, ok := in.(*os.File); ok && paste {
					flushInput(f) // the rest of the paste must not reach the code prompt, or the shell after
				}
				return b.Bytes(), nil
			}
		}
		if err == io.EOF {
			return b.Bytes(), nil
		}
		if err != nil {
			return nil, i18n.Err("migrate.read_failed", err)
		}
	}
}

// The ioctls of tcflush(TCIFLUSH). x/sys names each only on its own platform, and no file here has a build
// constraint: the i18n checks type-check every file as one package.
const (
	darwinTIOCFLUSH = 0x80047410 // takes a pointer to FREAD (1)
	linuxTCFLSH     = 0x540b     // takes TCIFLUSH (0); amd64 and arm64
)

// flushInput drops what the terminal f received and nobody read yet.
func flushInput(f *os.File) error {
	if runtime.GOOS == "darwin" {
		return unix.IoctlSetPointerInt(int(f.Fd()), darwinTIOCFLUSH, 1)
	}
	return unix.IoctlSetInt(int(f.Fd()), linuxTCFLSH, 0)
}

// importBundle imports b with the code of --code, else of the prompt, which asks again after a wrong one,
// three tries in all. On success it deletes file (if any): the login lives here now.
func importBundle(cmd *cobra.Command, b []byte, file string) error {
	code, _ := cmd.Flags().GetString("code")
	typed := !cmd.Flags().Changed("code")
	replace, _ := cmd.Flags().GetBool("replace")
	cl, err := client()
	if err != nil {
		return err
	}
	var x *api.MigrateImport
	for try := 1; ; try++ {
		if typed {
			if code, err = askCode(i18n.T("migrate.code_prompt")); err != nil {
				return err
			}
		}
		var wrong bool
		if x, wrong, err = sendImport(cmd, cl, b, code, replace); err == nil {
			break
		}
		if !wrong || !typed || try == 3 {
			return err
		}
		fmt.Fprintln(cmd.ErrOrStderr(), err)
	}
	out := cmd.OutOrStdout()
	for _, a := range x.Accounts {
		if a.NeedsLogin {
			fmt.Fprintln(out, i18n.T("migrate.imported_needs_login", a.Name, a.Email))
		} else {
			fmt.Fprintln(out, i18n.T("migrate.imported_one", a.Name, a.Email))
		}
		if a.Error != nil {
			fmt.Fprintln(out, i18n.T("migrate.imported_error", a.Name, *a.Error))
		}
	}
	if file == "" {
		return nil
	}
	if err := os.Remove(file); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), i18n.T("migrate.bundle_not_deleted", file, err))
	} else {
		fmt.Fprintln(out, i18n.T("migrate.bundle_deleted", file))
	}
	return nil
}

// sendImport imports b with code; wrong tells a code that is malformed or does not open b.
func sendImport(cmd *cobra.Command, cl *api.ClientWithResponses, b []byte, code string, replace bool) (x *api.MigrateImport, wrong bool, err error) {
	if _, err := migrate.ParseCode(code); err != nil {
		return nil, true, i18n.Err("daemon.invalid_code")
	}
	ctx, cancel := within(cmd, answerTimeout)
	defer cancel()
	r, err := cl.MigrateImportWithResponse(ctx, api.MigrateImportRequest{Bundle: string(b), Code: code, Replace: replace})
	if err != nil {
		if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
			return nil, false, down(err) // never sent
		}
		return nil, false, unknownErr{down(err)}
	}
	if r.StatusCode() != 200 {
		return nil, parseErr(r.Body).Code == "bad_bundle", migrateErr(r.StatusCode(), r.Body)
	}
	if r.JSON200 == nil {
		return nil, false, unknownErr{errBadResp}
	}
	return r.JSON200, false, nil
}

// unknownErr is the failure of an import sent without an answer: it may have landed, and its refresh spent
// the bundle's login. Its text, the transport's, can be raw English: the advice it brings is shown instead.
type unknownErr struct{ err error }

func (e unknownErr) Error() string { return e.err.Error() }

// shellQuote quotes p for a shell: the macOS home of ccswap has a space in it.
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// migrateCodes are the daemon's refusals of a migrate call whose message names the accounts and what to do:
// only the daemon knows which of several names failed.
var migrateCodes = []string{"invalid_name", "not_found", "migrating", "migrate_active", "target_refreshing", "needs_login",
	"duplicate", "tokens_changed", "bundle_spent", "bundle_unchecked"}

// migrateTexts are its refusals that name no account: the CLI's own texts, in the terminal's language.
var migrateTexts = map[string]error{
	"not_bundle":     i18n.Err("daemon.not_bundle"),
	"bundle_version": i18n.Err("daemon.bundle_version"),
	"invalid_code":   i18n.Err("daemon.invalid_code"),
	"bad_bundle":     i18n.Err("daemon.bad_bundle"),
	"export_gone":    i18n.Err("daemon.export_gone"),
	"export_done":    i18n.Err("daemon.export_done"),
}

// migrateErr is the error of a migrate call the daemon answered with an error: the text of a migrate
// refusal, the usual hint otherwise.
func migrateErr(code int, body []byte) error {
	e := parseErr(body)
	if err, ok := migrateTexts[e.Code]; ok {
		return err
	}
	if e.Message != "" && slices.Contains(migrateCodes, e.Code) {
		return errors.New(e.Message)
	}
	return apiErr(code, body, "")
}
