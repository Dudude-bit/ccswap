package cli

import (
	"path/filepath"

	"github.com/Dudude-bit/ccswap/internal/claudecode"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/keychain"
	"github.com/Dudude-bit/ccswap/internal/launchd"
	"github.com/Dudude-bit/ccswap/internal/notify"
	"github.com/Dudude-bit/ccswap/internal/systemd"
)

// platformKeychain is the secret store of the platform (claudecode.GOOS is the one seam).
func platformKeychain(home string) keychain.Store {
	if claudecode.GOOS == "linux" {
		return keychain.NewFiles(filepath.Join(home, "vault"))
	}
	return keychain.NewSecurity()
}

func platformNotifier() notify.Notifier {
	if claudecode.GOOS == "linux" {
		return notify.None{}
	}
	return notify.Osascript{}
}

// serviceManager is the daemon supervisor of the platform. Name is its noun for messages; the words
// after it have their own keys per platform, so a language can make them agree with it.
type serviceManager struct {
	Name          string
	Label         string
	Installed     string // "installed and running"
	Removed       string
	LoadedText    string // "loaded"
	NotLoadedText string
	Install       func(bin, home string) error
	Uninstall     func() error
	Loaded        func() (bool, error)
	ProgramPath   func() (string, error)
}

func platformService() serviceManager {
	if claudecode.GOOS == "linux" {
		return serviceManager{i18n.T("systemd.service"), systemd.Label, i18n.T("systemd.installed_running"),
			i18n.T("systemd.removed"), i18n.T("systemd.loaded"), i18n.T("systemd.not_loaded"),
			systemd.Install, systemd.Uninstall, systemd.Loaded, systemd.ProgramPath}
	}
	return serviceManager{"LaunchAgent", launchd.Label, i18n.T("launchd.installed_running"),
		i18n.T("launchd.removed"), i18n.T("launchd.loaded"), i18n.T("launchd.not_loaded"),
		launchd.Install, launchd.Uninstall, launchd.Loaded, launchd.ProgramPath}
}
