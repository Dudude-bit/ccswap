package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

// newSignalCmd is a Claude Code hook: it must never print, fail or hang a session.
func newSignalCmd() *cobra.Command {
	return &cobra.Command{
		Use: "signal", Short: i18n.T("signal.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 2500*time.Millisecond)
			defer cancel()
			in := cmd.InOrStdin()
			ch := make(chan []byte, 1)
			go func() { b, _ := io.ReadAll(io.LimitReader(in, 1<<20)); ch <- b }()
			var b []byte
			select {
			case b = <-ch:
			case <-ctx.Done():
				return nil
			}
			var payload map[string]any
			if json.Unmarshal(b, &payload) != nil || payload == nil {
				return nil
			}
			if !managedSession() {
				return nil
			}
			cl, err := client()
			if err != nil {
				return nil
			}
			_, _ = cl.SignalWithResponse(ctx, api.SignalRequest{Payload: payload})
			return nil
		},
	}
}

// managedSession reports whether the hook's session authenticates with the keychain entry ccswap
// switches: a session on an OAuth token or API key from the environment, behind a proxy, or
// on another config dir must not blame the active account for its limits.
func managedSession() bool {
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		if os.Getenv(k) != "" {
			return false
		}
	}
	home, err := config.Home()
	if err != nil {
		return false
	}
	cfg, err := config.Load(home)
	return err == nil && claudecode.ServiceName(os.Getenv("CLAUDE_CONFIG_DIR")) == claudecode.ServiceName(cfg.Claude.ConfigDir)
}
