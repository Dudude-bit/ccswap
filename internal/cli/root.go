// Package cli is the ccswap command line: a thin client of the daemon API.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/daemon"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/tui"
)

// ExitError asks main to exit with Code after printing Err (if any).
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

var errDown error = i18n.Err("cli.daemon_down")

var errBadResp error = i18n.Err("cli.bad_reply")

// newKeychain is a seam for tests.
var newKeychain = func(home string) keychain.Store { return platformKeychain(home) }

func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "ccswap",
		Short:         i18n.T("cli.short"),
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newLoginCmd(), newAdoptCmd(), newRemoveCmd(), newUseCmd(), newListCmd(), newStatusCmd(),
		newPriorityCmd(), newEnableCmd(true), newEnableCmd(false), newAutoCmd(), newStrategyCmd(), newMigrateCmd(),
		newT3Cmd(), newWebCmd(), newRemoteCmd(), newLangCmd(), newSignalCmd(), newDaemonCmd(), newDoctorCmd(), newTUICmd(),
	)
	localizeHelp(root)
	return root
}

// Execute runs ccswap in the language config.toml pins, else the locale's.
func Execute(ctx context.Context, version string) error {
	UseLang()
	return execute(ctx, NewRoot(version))
}

// stderrIsTerminal is a seam for tests.
var stderrIsTerminal = func() bool { return term.IsTerminal(os.Stderr.Fd()) }

// execute runs root. An error of daemon run, its own or cobra's, is English off a terminal: main prints it to
// stderr, which launchd and systemd keep in daemon.log.
func execute(ctx context.Context, root *cobra.Command) error {
	cmd, err := root.ExecuteContextC(ctx)
	if err != nil && cmd != nil && cmd.CommandPath() == "ccswap daemon run" && !stderrIsTerminal() {
		i18n.Set(i18n.EN)
	}
	return localizeCobra(err)
}

func UseLang() {
	pinned := ""
	if home, err := config.Home(); err == nil {
		if cfg, err := config.Load(home); err == nil {
			pinned = cfg.UI.Lang
		}
	}
	i18n.Set(i18n.Resolve(pinned, os.Getenv))
}

// localizeHelp translates what cobra adds by itself: the help and version flags, the help and completion
// commands and the headings of --help.
func localizeHelp(root *cobra.Command) {
	root.Flags().BoolP("version", "v", false, i18n.T("cobra.version_flag"))
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().BoolP("help", "h", false, i18n.T("cobra.help_flag", c.Name()))
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	root.SetUsageTemplate(strings.NewReplacer(
		`Use "{{.CommandPath}} [command] --help" for more information about a command.`, i18n.T("cobra.more_info"),
		"Usage:", i18n.T("cobra.usage"), "[command]", i18n.T("cobra.command_arg"), "Aliases:", i18n.T("cobra.aliases"),
		"Examples:", i18n.T("cobra.examples"), "Available Commands:", i18n.T("cobra.available_commands"),
		"Additional Commands:", i18n.T("cobra.additional_commands"), "Global Flags:", i18n.T("cobra.global_flags"),
		"Flags:", i18n.T("cobra.flags"), "Additional help topics:", i18n.T("cobra.help_topics"),
	).Replace(root.UsageTemplate()))
	for _, c := range root.Commands() {
		switch c.Name() {
		case "help":
			if s := i18n.T("cobra.help_short"); s != c.Short {
				c.Short, c.Long = s, "" // cobra's long text has no key: the translated short one stands in
			}
		case "completion":
			c.Short = i18n.T("cobra.completion_short")
			for _, sh := range c.Commands() {
				sh.Short = i18n.T("cobra.completion_shell_short", sh.Name())
			}
		}
	}
}

// cobraErrs are the errors cobra and its flag parser write in English, each with its message; the
// submatches are its arguments.
var cobraErrs = []struct {
	re *regexp.Regexp
	t  func(...any) string
}{
	{regexp.MustCompile(`^accepts (\d+) arg\(s\), received (\d+)$`), func(a ...any) string { return i18n.T("cobra.accepts_args", a...) }},
	{regexp.MustCompile(`^accepts at most (\d+) arg\(s\), received (\d+)$`), func(a ...any) string { return i18n.T("cobra.accepts_at_most_args", a...) }},
	{regexp.MustCompile(`^requires at least (\d+) arg\(s\), only received (\d+)$`), func(a ...any) string { return i18n.T("cobra.requires_at_least_args", a...) }},
	{regexp.MustCompile(`(?s)^unknown command "(.*?)" for "(.*?)"(.*)$`), func(a ...any) string { return i18n.T("cobra.unknown_command", a...) }},
	{regexp.MustCompile(`^unknown flag: (.*)$`), func(a ...any) string { return i18n.T("cobra.unknown_flag", a...) }},
	{regexp.MustCompile(`^unknown shorthand flag: (.*)$`), func(a ...any) string { return i18n.T("cobra.unknown_shorthand_flag", a...) }},
	{regexp.MustCompile(`^flag needs an argument: (.*)$`), func(a ...any) string { return i18n.T("cobra.flag_needs_argument", a...) }},
	{regexp.MustCompile(`^invalid argument (.*) for (.*) flag: (.*)$`), func(a ...any) string { return i18n.T("cobra.invalid_flag_argument", a...) }},
}

// localizeCobra translates the errors cobra and its flag parser write in English.
func localizeCobra(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, c := range cobraErrs {
		m := c.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		args := make([]any, len(m)-1)
		for i, s := range m[1:] {
			args[i] = s
		}
		if s := strings.Replace(c.t(args...), "Did you mean this?", i18n.T("cobra.did_you_mean"), 1); s != msg {
			return errors.New(s)
		}
		return err
	}
	return err
}

func socketPath() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	return config.SocketPath(home)
}

func client() (*api.ClientWithResponses, error) {
	sock, err := socketPath()
	if err != nil {
		return nil, err
	}
	return daemon.Dial(sock)
}

// down turns a connect failure into the «daemon is not running» message and a timeout into «slow».
func down(err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED):
		return errDown
	case tui.Timeout(err):
		return tui.ErrSlow
	}
	return err
}

// Reads are quick; a mutating call may wait on the daemon's switcher lock through an Anthropic call.
var readTimeout, writeTimeout = 10 * time.Second, 60 * time.Second

func within(cmd *cobra.Command, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), d)
}

// apiErr converts an unexpected response into an error; nil when code is one of ok.
func apiErr(code int, body []byte, name string, ok ...int) error {
	for _, c := range ok {
		if code == c {
			return nil
		}
	}
	e := parseErr(body)
	if h := tui.Describe(e, name); h != "" {
		return errors.New(h)
	}
	if e.Message == "" {
		return errors.New(i18n.T("cli.daemon_replied", code))
	}
	return errors.New(e.Message)
}

func parseErr(body []byte) api.Error {
	var e api.Error
	_ = jsonUnmarshal(body, &e)
	return e
}
