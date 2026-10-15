package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func newRemoteCmd() *cobra.Command {
	r := &cobra.Command{
		Use:   "remote",
		Short: i18n.T("remote.short"),
		Args:  cobra.NoArgs,
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
			out, rem := cmd.OutOrStdout(), r.JSON200.Remote
			switch {
			case rem == nil:
				fmt.Fprintln(out, offText())
			case rem.Ok && rem.CheckedAt != nil:
				fmt.Fprintf(out, i18n.T("remote.from_last_answer"), rem.Url, rem.CheckedAt.Local().Format("15:04:05"))
			case rem.Error != nil:
				fmt.Fprintf(out, i18n.T("remote.unreachable"), rem.Url, *rem.Error)
			default:
				fmt.Fprintf(out, i18n.T("remote.from_no_answer"), rem.Url)
			}
			return nil
		},
	}
	r.AddCommand(&cobra.Command{
		Use:   i18n.T("remote.set_use"),
		Short: i18n.T("remote.set_short"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := readKey(cmd, args[0])
			if err != nil {
				return err
			}
			req := api.RemoteRequest{Url: args[0]}
			if key != "" {
				req.Key = &key
			}
			if err := setRemote(cmd, req); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), i18n.T("remote.set_done"), args[0])
			return nil
		},
	})
	r.AddCommand(&cobra.Command{
		Use:   "off",
		Short: i18n.T("remote.off_short"),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := setRemote(cmd, api.RemoteRequest{}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), offText())
			return nil
		},
	})
	return r
}

// readKey reads the other ccswap's key without echo from a terminal, else the first line of stdin;
// "" keeps the stored one.
func readKey(cmd *cobra.Command, url string) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(f.Fd()) {
		fmt.Fprintf(cmd.ErrOrStderr(), i18n.T("remote.key_prompt"), url)
		b, err := term.ReadPassword(f.Fd())
		fmt.Fprintln(cmd.ErrOrStderr())
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func offText() string {
	return i18n.T("remote.off")
}

func setRemote(cmd *cobra.Command, req api.RemoteRequest) error {
	cl, err := client()
	if err != nil {
		return err
	}
	ctx, cancel := within(cmd, writeTimeout)
	defer cancel()
	r, err := cl.SetRemoteWithResponse(ctx, req)
	if err != nil {
		return down(err)
	}
	return apiErr(r.StatusCode(), r.Body, "", 204)
}
