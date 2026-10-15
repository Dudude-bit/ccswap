package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func strategyText(s api.Strategy) string {
	if s == api.CacheAware {
		return i18n.T("strategy.cache_aware")
	}
	return i18n.T("strategy.fill_first")
}

func newStrategyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "strategy [fill-first|cache-aware]",
		Short: i18n.T("strategy.short"),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
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
				fmt.Fprintf(out, "%s: %s\n", r.JSON200.Strategy, strategyText(r.JSON200.Strategy))
				return nil
			}
			s := api.Strategy(args[0])
			if !s.Valid() {
				return errors.New(i18n.T("strategy.unknown", args[0]))
			}
			ctx, cancel := within(cmd, writeTimeout)
			defer cancel()
			r, err := cl.SetStrategyWithResponse(ctx, api.StrategyRequest{Strategy: s})
			if err != nil {
				return down(err)
			}
			if err := apiErr(r.StatusCode(), r.Body, "", 204); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s: %s\n", s, strategyText(s))
			return nil
		},
	}
}
