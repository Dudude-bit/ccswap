package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/config"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func newLangCmd() *cobra.Command {
	return &cobra.Command{
		Use:   i18n.T("lang.use"),
		Short: i18n.T("lang.short"),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := config.Home()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				cfg, err := config.Load(home)
				if err != nil {
					return err
				}
				printLang(out, cfg.UI.Lang)
				var langs []string
				for _, l := range i18n.Available() {
					langs = append(langs, fmt.Sprintf("%s (%s)", l, i18n.Name(l)))
				}
				fmt.Fprintln(out, i18n.T("lang.available", strings.Join(langs, ", ")))
				return nil
			}
			pin := args[0]
			if pin == "auto" {
				pin = ""
			}
			pin, ok := i18n.Pin(pin)
			if !ok {
				return errors.New(i18n.T("lang.unknown", args[0], codes(", ")))
			}
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.SetLangWithResponse(ctx, api.LangRequest{Lang: pin})
			switch {
			case err != nil && errors.Is(down(err), errDown):
				if err := config.SaveLang(home, pin); err != nil { // the daemon reads it when it starts
					return err
				}
			case err != nil:
				return down(err)
			default:
				if err := apiErr(r.StatusCode(), r.Body, "", 204); err != nil {
					return err
				}
			}
			i18n.Set(i18n.Resolve(pin, os.Getenv))
			printLang(out, pin)
			return nil
		},
	}
}

func printLang(out io.Writer, pin string) {
	name := i18n.Name(i18n.Resolve(pin, os.Getenv))
	if pin == "" {
		fmt.Fprintln(out, i18n.T("lang.from_locale", name))
		return
	}
	fmt.Fprintln(out, i18n.T("lang.pinned", name))
}

// codes are the languages ccswap has, joined by sep.
func codes(sep string) string {
	var s []string
	for _, l := range i18n.Available() {
		s = append(s, string(l))
	}
	return strings.Join(s, sep)
}
