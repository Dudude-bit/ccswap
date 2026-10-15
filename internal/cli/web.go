package cli

import (
	"errors"
	"fmt"
	"net"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/web"
)

// openURL opens the page on a Mac; a server has no browser to open. Test seam.
var openURL = func(u string) error {
	if claudecode.GOOS != "darwin" {
		return nil
	}
	return exec.Command("open", u).Run()
}

func newWebCmd() *cobra.Command {
	return &cobra.Command{
		Use: "web", Short: i18n.T("page.short"), Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			ctx, cancel := within(cmd, readTimeout)
			defer cancel()
			st, err := cl.GetStatusWithResponse(ctx)
			if err != nil {
				return down(err)
			}
			if err := apiErr(st.StatusCode(), st.Body, "", 200); err != nil {
				return err
			}
			if st.JSON200 == nil {
				return errBadResp
			}
			if st.JSON200.Listen == nil {
				if v, this := st.JSON200.Version, cmd.Root().Version; v != this { // one from before `listen` never says it
					return errors.New(i18n.T("page.daemon_version_differs", v, this))
				}
				return errors.New(i18n.T("page.off"))
			}
			listen := *st.JSON200.Listen // where the daemon listens, whatever config.toml says since
			links, local, err := listenLinks(listen, web.Page)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			link := links[0]
			if local {
				r, err := cl.CreateWebCodeWithResponse(ctx)
				if err != nil {
					return down(err)
				}
				if err := apiErr(r.StatusCode(), r.Body, "", 200); err != nil {
					return err
				}
				if r.JSON200 == nil {
					return errBadResp
				}
				link += "#code=" + r.JSON200.Code
				fmt.Fprintln(out, link)
				fmt.Fprintln(out, i18n.T("page.link_once"))
				if claudecode.GOOS != "darwin" {
					_, port, _ := net.SplitHostPort(listen)
					fmt.Fprintf(out, i18n.T("page.ssh_tunnel_hint"), port, listen)
				}
			} else {
				for _, l := range links {
					fmt.Fprintln(out, l)
				}
				fmt.Fprintln(out, i18n.T("page.read_only_hint"))
			}
			if err := openURL(link); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), i18n.T("page.browser_failed"), err)
			}
			return nil
		},
	}
}

// listenLinks are the links to path on the listener: the listen address, or for a listener on all
// interfaces a loopback link first, then one per non-loopback IPv4 address. local: the listener is on
// a loopback address, the only one where the page may change settings.
func listenLinks(listen, path string) (links []string, local bool, err error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, false, fmt.Errorf(i18n.T("page.bad_listen"), listen, err)
	}
	link := func(h string) string { return "http://" + net.JoinHostPort(h, port) + path }
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{link(host)}, host == "localhost" || ip != nil && ip.IsLoopback(), nil
	}
	links = []string{link("127.0.0.1")}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			links = append(links, link(n.IP.String()))
		}
	}
	return links, false, nil
}
