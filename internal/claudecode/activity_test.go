package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLastActivity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	if at, err := LastActivity(dir); err != nil || !at.IsZero() {
		t.Fatal("missing projects dir:", at, err)
	}
	newest := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	for name, mod := range map[string]time.Time{
		"-Users-x-a/1.jsonl":                   newest.Add(-time.Hour),
		"-Users-x-b/2/subagents/agent-1.jsonl": newest,
		"-Users-x-b/notes.txt":                 newest.Add(time.Hour),
	} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("{}\n"), 0o600)
		os.Chtimes(p, mod, mod)
	}
	if at, err := LastActivity(dir); err != nil || !at.Equal(newest) {
		t.Fatalf("got %v %v, want %v", at, err, newest)
	}
	if got := ProjectsDir("/u", ""); got != "/u/.claude/projects" {
		t.Fatal(got)
	}
	if got := ProjectsDir("/u", "/cfg"); got != "/cfg/projects" {
		t.Fatal(got)
	}
}
