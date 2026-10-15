package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/t3"
)

// listener returns the config and the read key of the daemon's HTTP listener.
func listener() (config.Config, string, error) {
	home, err := config.Home()
	if err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return config.Config{}, "", err
	}
	key, err := newKeychain(home).Get(store.VaultService, t3.KeyAccount)
	if errors.Is(err, keychain.ErrNotFound) || err == nil && len(key) == 0 {
		return cfg, "", errors.New(i18n.T("t3.no_key"))
	}
	return cfg, string(key), err
}

func newT3Cmd() *cobra.Command {
	return &cobra.Command{
		Use: "t3", Short: i18n.T("t3.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, key, err := listener()
			if err != nil {
				return err
			}
			links, _, err := listenLinks(cfg.T3.Listen, "") // on all interfaces: another machine needs a real address
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !cfg.T3.Enabled {
				fmt.Fprintln(out, i18n.T("t3.off"))
			}
			for _, l := range links {
				fmt.Fprintf(out, i18n.T("t3.address"), l)
			}
			fmt.Fprintf(out, i18n.T("t3.key"), key)
			fmt.Fprintln(out, i18n.T("t3.how_to_add"))
			fmt.Fprintln(out, i18n.T("t3.read_only_hint"))
			return nil
		},
	}
}
