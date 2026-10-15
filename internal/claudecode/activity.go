package claudecode

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// ProjectsDir holds Claude Code's session transcripts.
func ProjectsDir(home, configDir string) string {
	return filepath.Join(ConfigDir(home, configDir), "projects")
}

// LastActivity is the newest transcript write under dir, Claude Code's last request as far as
// anything on disk tells; zero when there is no transcript.
func LastActivity(dir string) (time.Time, error) {
	var last time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) { // a transcript removed mid-walk
			return nil
		}
		if err != nil {
			return err
		}
		if fi.ModTime().After(last) {
			last = fi.ModTime()
		}
		return nil
	})
	return last, err
}
