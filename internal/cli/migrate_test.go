package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dudude-bit/ccswap/internal/api"
	"github.com/Dudude-bit/ccswap/internal/daemon"
	"github.com/Dudude-bit/ccswap/internal/i18n"
	"github.com/Dudude-bit/ccswap/internal/migrate"
	"github.com/Dudude-bit/ccswap/internal/store"
	"github.com/Dudude-bit/ccswap/internal/switcher"
)

const (
	testBundle = "-----BEGIN CCSWAP BUNDLE v1-----\n" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + "\n-----END CCSWAP BUNDLE-----\n"
	testCode   = "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567"
	testAfter  = "14:06:30" // the export's deadline plus a minute, local time
)

func exportFake() *fake {
	return &fake{export: api.MigrateExport{Token: "tok", Bundle: testBundle, Code: testCode,
		ExpiresAt: time.Date(2026, 10, 15, 14, 5, 30, 0, time.Local)}}
}

// exitCode is the code of an *ExitError, -1 for any other error: a plain error would print one more line.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*ExitError](err); ok {
		return ee.Code
	}
	return -1
}

// hang makes the fake's commit and import (and cancel, when cancelToo) outlast a short answerTimeout: an
// unknown outcome. The export itself keeps its own timeout.
func hang(t *testing.T, f *fake, cancelToo bool) {
	t.Helper()
	old := answerTimeout
	answerTimeout = 200 * time.Millisecond
	block := make(chan struct{})
	f.blockCommit, f.blockImport = block, block
	if cancelToo {
		f.blockCancel = block
	}
	t.Cleanup(func() { close(block); answerTimeout = old }) // before env's cleanup stops the server
}

func writeState(t *testing.T, home string, names ...string) {
	t.Helper()
	var as []string
	for _, n := range names {
		as = append(as, `{"name":"`+n+`"}`)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"accounts":[`+strings.Join(as, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// saved is the one bundle saved under <home>/migrate, checked private and holding want.
func saved(t *testing.T, home, prefix, want string) string {
	t.Helper()
	ps, _ := filepath.Glob(filepath.Join(home, "migrate", prefix+"-*.ccswap"))
	if len(ps) != 1 {
		t.Fatalf("saved bundles: %v", ps)
	}
	if b, _ := os.ReadFile(ps[0]); string(b) != want {
		t.Fatalf("saved %q", b)
	}
	if fi, _ := os.Stat(ps[0]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(ps[0])); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	return ps[0]
}

func TestExportToFile(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	file := filepath.Join(home, "out.ccswap")
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE}
	var ignored []bool
	for _, s := range sigs {
		ignored = append(ignored, signal.Ignored(s))
	}
	stdout, stderr, err := runSplit(t, "", "migrate", "export", "a", "b", "a", "--yes", "-o", file) // a twice is a once
	if err != nil {
		t.Fatal(err, stderr)
	}
	// only the signals live before: Stop puts back an inherited SIG_IGN, which Ignored cannot see
	for i, s := range sigs {
		if !ignored[i] && signal.Ignored(s) {
			t.Errorf("%v: ignored after the export, not before", s)
		}
	}
	if b, _ := os.ReadFile(file); string(b) != testBundle {
		t.Fatalf("file %q", b)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if c := f.migrateCalls(); c != "export commit" {
		t.Fatalf("calls %q", c)
	}
	if stdout != "" || strings.Count(stderr, testCode) != 1 || !strings.Contains(stderr, i18n.T("migrate.exported_file", "a, b", file)) ||
		!strings.Contains(stderr, i18n.T("migrate.import_once")) || strings.Contains(stderr, i18n.T("migrate.pipe_warning")) {
		t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
	}
}

func TestExportRefusesExistingFile(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	file, link := filepath.Join(home, "old"), filepath.Join(home, "dangling")
	os.WriteFile(file, []byte("old"), 0o600)
	os.Symlink(filepath.Join(home, "nowhere"), link)
	for _, p := range []string{file, link} {
		_, _, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", p)
		if err == nil || err.Error() != i18n.T("migrate.file_exists", p) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if c := f.migrateCalls(); c != "" {
		t.Fatalf("calls %q", c)
	}
	if b, _ := os.ReadFile(file); string(b) != "old" {
		t.Fatalf("file %q", b)
	}
}

func TestExportAsksFirst(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	file := filepath.Join(home, "out.ccswap")
	stdout, stderr, err := runSplit(t, "n\n", "migrate", "export", "a", "-o", file)
	if err != nil || f.migrateCalls() != "" || stdout != "" ||
		!strings.Contains(stderr, i18n.T("migrate.confirm", "a")) || !strings.Contains(stderr, i18n.T("migrate.cancelled_by_user")) {
		t.Fatalf("declined: %v %q\nstdout %q\nstderr %s", err, f.migrateCalls(), stdout, stderr)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("file written after a no")
	}
	if _, stderr, err := runSplit(t, "y\n", "migrate", "export", "a", "-o", file); err != nil || f.migrateCalls() != "export commit" {
		t.Fatalf("accepted: %v %q %s", err, f.migrateCalls(), stderr)
	}
	// the risk of a pipe is told before the consent, not after
	_, stderr, err = runSplit(t, "n\n", "migrate", "export", "a")
	w, q := strings.Index(stderr, i18n.T("migrate.pipe_warning")), strings.Index(stderr, i18n.T("migrate.confirm", "a"))
	if err != nil || w < 0 || q < 0 || w > q || f.migrateCalls() != "export commit" {
		t.Fatalf("pipe: %v %q\n%s", err, f.migrateCalls(), stderr)
	}
}

func TestExportCommitRefusedDeletesFile(t *testing.T) {
	f := exportFake()
	f.commitErr = i18n.Err("daemon.tokens_changed", daemon.ErrTokensChanged, "a")
	home := env(t, f)
	file := filepath.Join(home, "out.ccswap")
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
	if exitCode(err) != 1 {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("bundle of a refused commit left behind")
	}
	if !strings.Contains(stderr, i18n.T("daemon.tokens_changed", daemon.ErrTokensChanged, "a")) ||
		!strings.Contains(stderr, i18n.T("migrate.export_cancelled")) || strings.Contains(stderr, testCode) {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestExportFileWriteFailureCancels(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the read-only dir this test needs")
	}
	f := exportFake()
	home := env(t, f)
	dir := filepath.Join(home, "ro")
	os.Mkdir(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	file := filepath.Join(dir, "out.ccswap")
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
	if exitCode(err) != 1 {
		t.Fatal(err)
	}
	if c := f.migrateCalls(); c != "export cancel" {
		t.Fatalf("calls %q", c)
	}
	werr := os.WriteFile(file, nil, 0o600) // the same refusal
	if werr == nil || !strings.Contains(stderr, i18n.T("migrate.write_failed", werr)) ||
		!strings.Contains(stderr, i18n.T("migrate.export_cancelled")) || strings.Contains(stderr, testCode) {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestExportPipe(t *testing.T) {
	f := exportFake()
	env(t, f)
	stdout, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes")
	if err != nil {
		t.Fatal(err, stderr)
	}
	if c := f.migrateCalls(); c != "export commit" {
		t.Fatalf("calls %q", c)
	}
	if stdout != testBundle || !strings.Contains(stderr, i18n.T("migrate.pipe_warning")) || strings.Count(stderr, testCode) != 1 ||
		!strings.Contains(stderr, i18n.T("migrate.import_once")) {
		t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
	}
}

type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, syscall.EPIPE }

func TestExportPipeWriteFailureSavesBundle(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	stderr, err := runTo(t, brokenPipe{}, "", "migrate", "export", "a", "--yes")
	if exitCode(err) != 1 || f.migrateCalls() != "export commit" {
		t.Fatalf("%v %q", err, f.migrateCalls())
	}
	p := saved(t, home, "export", testBundle)
	if !strings.Contains(stderr, i18n.T("migrate.pipe_failed_saved", p)) || strings.Count(stderr, testCode) != 1 ||
		!strings.Contains(stderr, i18n.T("migrate.import_once")) {
		t.Fatalf("stderr %s", stderr)
	}
}

// TestExportSurvivesRealBrokenPipe runs an export whose stdout is a pipe nobody reads: without SIGPIPE
// ignored, Go kills the process at the write and the moved login is lost with it.
func TestExportSurvivesRealBrokenPipe(t *testing.T) {
	if os.Getenv("CCSWAP_TEST_BROKEN_PIPE") == "1" {
		home := env(t, exportFake())
		os.Stderr.WriteString(home + "\n")
		root := NewRoot("test")
		root.SetArgs([]string{"migrate", "export", "a", "--yes"})
		os.Exit(exitCode(execute(t.Context(), root)))
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	var stderr bytes.Buffer
	c := exec.Command(os.Args[0], "-test.run=^TestExportSurvivesRealBrokenPipe$")
	c.Env = append(os.Environ(), "CCSWAP_TEST_BROKEN_PIPE=1")
	c.Stdout, c.Stderr = w, &stderr
	err = c.Run()
	w.Close()
	home, _, _ := strings.Cut(stderr.String(), "\n")
	if strings.HasPrefix(home, "/tmp/cc") {
		t.Cleanup(func() { os.RemoveAll(home) }) // the child exits past its own cleanup
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 1 {
		t.Fatalf("child: %v\n%s", err, stderr.String())
	}
	saved(t, home, "export", testBundle)
}

func TestExportSettlesUnknownCommitViaCancel(t *testing.T) {
	t.Run("export_done", func(t *testing.T) {
		f := exportFake()
		f.cancelErr = daemon.ErrExportDone
		home := env(t, f)
		hang(t, f, false)
		file := filepath.Join(home, "out.ccswap")
		_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
		if err != nil || f.migrateCalls() != "export commit cancel" {
			t.Fatalf("%v %q %s", err, f.migrateCalls(), stderr)
		}
		if b, _ := os.ReadFile(file); string(b) != testBundle {
			t.Fatalf("file %q", b)
		}
		if !strings.Contains(stderr, i18n.T("migrate.exported_file", "a", file)) || strings.Count(stderr, testCode) != 1 ||
			!strings.Contains(stderr, i18n.T("migrate.settling")) {
			t.Fatalf("stderr %s", stderr)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := exportFake()
		home := env(t, f)
		hang(t, f, false)
		file := filepath.Join(home, "out.ccswap")
		_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
		if exitCode(err) != 1 || f.migrateCalls() != "export commit cancel" {
			t.Fatalf("%v %q", err, f.migrateCalls())
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatal("bundle of a cancelled export left behind")
		}
		if !strings.Contains(stderr, i18n.T("migrate.export_cancelled")) || strings.Contains(stderr, testCode) ||
			!strings.Contains(stderr, i18n.T("migrate.settling")) {
			t.Fatalf("stderr %s", stderr)
		}
	})
}

func TestExportSettlesUnknownCommitViaState(t *testing.T) {
	home := env(t, nil)
	cl, err := daemon.Dial(filepath.Join(home, "gone.sock")) // the daemon is down
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		state []string // nil: no state.json
		want  outcome
	}{
		{[]string{"other"}, committed},
		{[]string{"a", "b", "other"}, notCommitted},
		{[]string{"a"}, unknown},
		{nil, unknown},
	} {
		os.Remove(filepath.Join(home, "state.json"))
		if c.state != nil {
			writeState(t, home, c.state...)
		}
		var w bytes.Buffer
		if got := settle(t.Context(), &w, cl, "tok", []string{"a", "b"}); got != c.want {
			t.Errorf("state %v: %v, want %v", c.state, got, c.want)
		}
	}

	// a daemon that restarted knows no export: the file of an undecidable one stays, with the code
	f := exportFake()
	f.cancelErr = daemon.ErrExportGone
	home = env(t, f)
	hang(t, f, false)
	writeState(t, home, "a")
	file := filepath.Join(home, "out.ccswap")
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "b", "--yes", "-o", file)
	if exitCode(err) != 1 || f.migrateCalls() != "export commit cancel" {
		t.Fatalf("%v %q", err, f.migrateCalls())
	}
	if b, _ := os.ReadFile(file); string(b) != testBundle {
		t.Fatalf("file %q", b)
	}
	if !strings.Contains(stderr, i18n.T("migrate.unknown_file", file, testAfter, "a, b")) || strings.Count(stderr, testCode) != 1 ||
		!strings.Contains(stderr, i18n.T("migrate.import_once")) {
		t.Fatalf("stderr %s", stderr)
	}
}

func TestExportUnknownPipeSaves(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	hang(t, f, true)
	stdout, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes")
	if exitCode(err) != 1 || f.migrateCalls() != "export commit cancel cancel cancel" {
		t.Fatalf("%v %q", err, f.migrateCalls())
	}
	p := saved(t, home, "export", testBundle)
	// the wait is told as it starts: the user must not kill ccswap meanwhile
	if stdout != "" || !strings.HasPrefix(stderr, i18n.T("migrate.pipe_warning")+"\n"+i18n.T("migrate.settling")+"\n") ||
		!strings.Contains(stderr, i18n.T("migrate.unknown_saved", p, testAfter, "a")) || strings.Count(stderr, testCode) != 1 ||
		!strings.Contains(stderr, i18n.T("migrate.import_once")) {
		t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
	}
}

func TestSaveBundleNeverOverwrites(t *testing.T) {
	home := env(t, nil)
	var ps []string
	for range 3 {
		p, err := saveBundle("received", []byte(testBundle))
		if err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
	}
	if ps[0] == ps[1] || ps[1] == ps[2] || ps[0] == ps[2] || !strings.HasPrefix(ps[0], filepath.Join(home, "migrate", "received-")) {
		t.Fatalf("paths %v", ps)
	}
}

func TestExportRejectsEmptyReply(t *testing.T) {
	f := exportFake()
	f.export.Bundle = ""
	home := env(t, f)
	file := filepath.Join(home, "out.ccswap")
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
	if !errors.Is(err, errBadResp) || f.migrateCalls() != "export cancel" || strings.Contains(stderr, testCode) {
		t.Fatalf("%v %q %s", err, f.migrateCalls(), stderr)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("empty bundle written")
	}
}

// TestExportPipeSaveFailure: a bundle stdout refused and no file took is shown on stderr with its code, as the
// last copy of the login.
func TestExportPipeSaveFailure(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	os.WriteFile(filepath.Join(home, "migrate"), nil, 0o600) // no dir can be made there
	stderr, err := runTo(t, brokenPipe{}, "", "migrate", "export", "a", "--yes")
	merr := os.MkdirAll(filepath.Join(home, "migrate"), 0o700) // the same refusal
	want := i18n.T("migrate.not_saved", merr, testAfter, "a") + "\n" + testBundle + i18n.T("migrate.code", testCode) + "\n" +
		i18n.T("migrate.import_once") + "\n"
	if exitCode(err) != 1 || merr == nil || !strings.HasSuffix(stderr, want) {
		t.Fatalf("%v %v\n%s", err, merr, stderr)
	}
}

func TestExportCancelledFileNotRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the read-only dir this test needs")
	}
	f := exportFake()
	home := env(t, f)
	dir := filepath.Join(home, "out")
	os.Mkdir(dir, 0o700)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	file := filepath.Join(dir, "out.ccswap")
	f.commitErr = i18n.Err("daemon.tokens_changed", daemon.ErrTokensChanged, "a")
	f.onCommit = func() { os.Chmod(dir, 0o500) }
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
	if exitCode(err) != 1 {
		t.Fatal(err)
	}
	rerr := os.Remove(file) // the same refusal
	if rerr == nil || !strings.Contains(stderr, i18n.T("migrate.remove_failed", rerr)) ||
		!strings.Contains(stderr, i18n.T("migrate.export_cancelled")) || strings.Contains(stderr, testCode) {
		t.Fatalf("%v\n%s", rerr, stderr)
	}
}

// answer stubs askCode with replies in turn, a code or an error (the last one repeats), and counts the asks.
func answer(t *testing.T, replies ...any) *int {
	t.Helper()
	asked := 0
	askCode = func(prompt string) (string, error) {
		if prompt != i18n.T("migrate.code_prompt") {
			t.Errorf("prompt %q", prompt)
		}
		r := replies[min(asked, len(replies)-1)]
		asked++
		if err, ok := r.(error); ok {
			return "", err
		}
		return r.(string), nil
	}
	return &asked
}

// noneSaved fails when an import kept a bundle under <home>/migrate.
func noneSaved(t *testing.T, home string) {
	t.Helper()
	if ps, _ := filepath.Glob(filepath.Join(home, "migrate", "*")); len(ps) != 0 {
		t.Fatalf("saved: %v", ps)
	}
}

func writeBundle(t *testing.T, home string) string {
	t.Helper()
	file := filepath.Join(home, "in.ccswap")
	if err := os.WriteFile(file, []byte(testBundle), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestImportFromFile(t *testing.T) {
	e := "Anthropic did not answer"
	f := &fake{imported: api.MigrateImport{Accounts: []api.Account{
		{Name: "a", Email: "a@x.y", Error: &e}, {Name: "b", Email: "b@x.y", NeedsLogin: true}}}}
	home := env(t, f)
	file := writeBundle(t, home)
	asked := answer(t, testCode)
	stdout, stderr, err := runSplit(t, "", "migrate", "import", file)
	if err != nil {
		t.Fatal(err, stderr)
	}
	if r := f.importReqs(); *asked != 1 || len(r) != 1 || r[0] != (api.MigrateImportRequest{Bundle: testBundle, Code: testCode}) {
		t.Fatalf("asked %d, requests %+v", *asked, r)
	}
	want := i18n.T("migrate.imported_one", "a", "a@x.y") + "\n" + i18n.T("migrate.imported_error", "a", e) + "\n" +
		i18n.T("migrate.imported_needs_login", "b", "b@x.y") + "\n" + i18n.T("migrate.bundle_deleted", file) + "\n"
	if stdout != want || stderr != "" {
		t.Fatalf("stdout %q\nstderr %q", stdout, stderr)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("bundle left after the import")
	}
	noneSaved(t, home)
}

func TestImportCodeFlag(t *testing.T) {
	f := &fake{imported: api.MigrateImport{Accounts: []api.Account{{Name: "a", Email: "a@x.y"}}}}
	home := env(t, f)
	asked := answer(t, "unused")
	stdout, stderr, err := runSplit(t, testBundle, "migrate", "import", "--code", testCode)
	if err != nil {
		t.Fatal(err, stderr)
	}
	if r := f.importReqs(); *asked != 0 || len(r) != 1 || r[0] != (api.MigrateImportRequest{Bundle: testBundle, Code: testCode}) {
		t.Fatalf("asked %d, requests %+v", *asked, r)
	}
	if stdout != i18n.T("migrate.imported_one", "a", "a@x.y")+"\n" || stderr != "" {
		t.Fatalf("stdout %q\nstderr %q", stdout, stderr)
	}
	noneSaved(t, home)
}

func TestImportReplaceFlag(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	file := writeBundle(t, home)
	answer(t, testCode)
	if _, stderr, err := runSplit(t, "", "migrate", "import", file, "--replace"); err != nil {
		t.Fatal(err, stderr)
	}
	if r := f.importReqs(); len(r) != 1 || r[0] != (api.MigrateImportRequest{Bundle: testBundle, Code: testCode, Replace: true}) {
		t.Fatalf("requests %+v", r)
	}
}

func TestImportRetriesWrongCode(t *testing.T) {
	bad := i18n.T("daemon.bad_bundle")
	f := &fake{importErrs: []error{migrate.ErrBadCode, migrate.ErrBadCode}}
	home := env(t, f)
	file := writeBundle(t, home)
	asked := answer(t, testCode)
	_, stderr, err := runSplit(t, "", "migrate", "import", file)
	if err != nil || *asked != 3 || f.migrateCalls() != "import import import" || strings.Count(stderr, bad) != 2 {
		t.Fatalf("%v: asked %d, calls %q\n%s", err, *asked, f.migrateCalls(), stderr)
	}

	// a malformed code costs a try too, without a call
	f = &fake{importErrs: []error{migrate.ErrBadCode}}
	home = env(t, f)
	file = writeBundle(t, home)
	asked = answer(t, "nonsense", testCode)
	_, stderr, err = runSplit(t, "", "migrate", "import", file)
	if err != nil || *asked != 3 || f.migrateCalls() != "import import" ||
		!strings.Contains(stderr, i18n.T("daemon.invalid_code")) || !strings.Contains(stderr, bad) {
		t.Fatalf("%v: asked %d, calls %q\n%s", err, *asked, f.migrateCalls(), stderr)
	}

	// --code is not asked again
	for _, c := range []struct{ code, calls, err string }{{testCode, "import", bad}, {"nonsense", "", i18n.T("daemon.invalid_code")}} {
		f = &fake{importErrs: []error{migrate.ErrBadCode}}
		home = env(t, f)
		file = writeBundle(t, home)
		asked = answer(t, testCode)
		_, _, err = runSplit(t, "", "migrate", "import", file, "--code", c.code)
		if err == nil || err.Error() != c.err || *asked != 0 || f.migrateCalls() != c.calls {
			t.Fatalf("--code %s: %v: asked %d, calls %q", c.code, err, *asked, f.migrateCalls())
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatal("bundle of a failed import deleted")
		}
	}
}

func TestImportPastedBundle(t *testing.T) {
	f := &fake{}
	env(t, f)
	answer(t, testCode)
	old := isTerminal
	isTerminal = func(v any) bool { _, ok := v.(*strings.Reader); return ok } // stdin
	t.Cleanup(func() { isTerminal = old })
	paste := "\r\nMove a to another machine? [y/N] y\r\n  \r\n" + strings.ReplaceAll(testBundle, "\n", "\r\n") + "\r\nccswap list\r\n"
	_, stderr, err := runSplit(t, paste, "migrate", "import")
	if err != nil {
		t.Fatal(err, stderr)
	}
	if r := f.importReqs(); len(r) != 1 || r[0].Bundle != testBundle || !strings.Contains(stderr, i18n.T("migrate.paste")) {
		t.Fatalf("requests %+v\n%s", r, stderr)
	}
}

func TestImportFromStdinSavesBundleOnFailure(t *testing.T) {
	dup := i18n.Err("daemon.migrate_email_here", store.ErrDuplicate, "a@x.y", "a")
	for _, c := range []struct {
		name    string
		in      string // testBundle when empty
		f       *fake  // nil: the daemon is down
		replies []any
		err     string
	}{
		{"no tty", "", &fake{}, []any{errNoTTY}, errNoTTY.Error()},
		{"interrupted", "", &fake{}, []any{errInterrupted}, errInterrupted.Error()},
		{"three wrong codes", "", &fake{importErrs: []error{migrate.ErrBadCode, migrate.ErrBadCode, migrate.ErrBadCode}}, []any{testCode}, i18n.T("daemon.bad_bundle")},
		{"duplicate", "", &fake{importErrs: []error{dup}}, []any{testCode}, dup.Error()},
		{"daemon down", "", nil, []any{testCode}, errDown.Error()},
		{"newer", strings.Replace(testBundle, "v1", "v2", 1), &fake{}, []any{testCode}, i18n.T("daemon.bundle_version")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.in == "" {
				c.in = testBundle
			}
			home := env(t, c.f)
			answer(t, c.replies...)
			stdout, stderr, err := runSplit(t, c.in, "migrate", "import")
			if exitCode(err) != 1 {
				t.Fatal(err)
			}
			p := saved(t, home, "received", c.in)
			if !strings.Contains(stderr, c.err+"\n"+i18n.T("migrate.bundle_saved", p, "ccswap migrate import "+shellQuote(p))) || stdout != "" {
				t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
			}

			// a file stays where it is, and nothing is saved
			os.RemoveAll(filepath.Join(home, "migrate"))
			if c.f != nil {
				c.f.mu.Lock()
				c.f.imports = nil // the same answers again
				c.f.mu.Unlock()
			}
			file := filepath.Join(home, "in.ccswap")
			os.WriteFile(file, []byte(c.in), 0o600)
			_, _, err = runSplit(t, "", "migrate", "import", file)
			if err == nil || err.Error() != c.err {
				t.Fatalf("file: %v", err)
			}
			noneSaved(t, home)
			if b, _ := os.ReadFile(file); string(b) != c.in {
				t.Fatalf("file %q", b)
			}
		})
	}
	if q := shellQuote("/a b/it's"); q != `'/a b/it'\''s'` {
		t.Fatalf("quoted %s", q)
	}
}

// TestImportUnknownOutcome: an import that went out without an answer may have landed, its refresh
// spending the bundle's login: the bundle is kept with ccswap list to tell, never a plain retry.
func TestImportUnknownOutcome(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	hang(t, f, false)
	answer(t, testCode)
	stdout, stderr, err := runSplit(t, testBundle, "migrate", "import")
	if exitCode(err) != 1 || f.migrateCalls() != "import" {
		t.Fatalf("%v %q", err, f.migrateCalls())
	}
	p := saved(t, home, "received", testBundle)
	// the advice alone: the transport's error can be raw English
	if want := i18n.T("migrate.import_unknown", p, "ccswap migrate import "+shellQuote(p)) + "\n"; stdout != "" || stderr != want {
		t.Fatalf("stdout %q\nstderr %s", stdout, stderr)
	}

	os.RemoveAll(filepath.Join(home, "migrate"))
	file := writeBundle(t, home)
	_, stderr, err = runSplit(t, "", "migrate", "import", file)
	if want := i18n.T("migrate.import_unknown", file, "ccswap migrate import "+shellQuote(file)) + "\n"; exitCode(err) != 1 || stderr != want {
		t.Fatalf("file: %v\n%s", err, stderr)
	}
	noneSaved(t, home)
	if b, _ := os.ReadFile(file); string(b) != testBundle {
		t.Fatalf("file %q", b)
	}

	// nowhere to save it: the advice still comes, before the bundle on stdout
	os.WriteFile(filepath.Join(home, "migrate"), nil, 0o600)
	stdout, stderr, err = runSplit(t, testBundle, "migrate", "import")
	merr := os.MkdirAll(filepath.Join(home, "migrate"), 0o700) // the same refusal
	want := i18n.T("migrate.import_unknown_shown") + "\n" + i18n.T("migrate.save_failed", merr) + "\n"
	if exitCode(err) != 1 || merr == nil || stdout != testBundle || stderr != want {
		t.Fatalf("unsaved: %v %v\nstdout %q\nstderr %s", err, merr, stdout, stderr)
	}
}

// TestImportIgnoresTextAround: a shell banner before the bundle on a pipe, or junk around it in a file, is
// not the bundle's.
func TestImportIgnoresTextAround(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	answer(t, testCode)
	in := "Welcome to old-mac\nLast login: today\n" + testBundle + "\nbye\n"
	file := filepath.Join(home, "in.ccswap")
	os.WriteFile(file, []byte(in), 0o600)
	if _, stderr, err := runSplit(t, in, "migrate", "import"); err != nil {
		t.Fatal(err, stderr)
	}
	if _, stderr, err := runSplit(t, "", "migrate", "import", file); err != nil {
		t.Fatal(err, stderr)
	}
	// a banner with no newline of its own, and text right after END
	glued := "Welcome" + strings.Replace(testBundle, migrate.EndLine+"\n", migrate.EndLine+"bye\n", 1)
	if _, stderr, err := runSplit(t, glued, "migrate", "import"); err != nil {
		t.Fatal(err, stderr)
	}
	if r := f.importReqs(); len(r) != 3 || r[0].Bundle != testBundle || r[1].Bundle != testBundle || r[2].Bundle != testBundle {
		t.Fatalf("requests %+v", r)
	}
}

// TestFlushInput: what a paste left after the END line is dropped from the terminal's input, so it never
// reaches the code prompt or the shell.
func TestFlushInput(t *testing.T) {
	master, tty := openPTY(t)
	typeIn(t, master, tty, "ccswap list\n")
	if err := flushInput(tty); err != nil {
		t.Fatal(err)
	}
	if got := typedNext(t, master, tty); got != "typed\n" {
		t.Fatalf("read %q", got)
	}
}

// TestImportPasteAtTerminalFlushesTheRest: readBundle reading a real terminal flushes what the paste had
// after its END line.
func TestImportPasteAtTerminalFlushesTheRest(t *testing.T) {
	master, tty := openPTY(t)
	typeIn(t, master, tty, testBundle+"ccswap list\n")
	cmd := &cobra.Command{}
	cmd.SetIn(tty)
	var errb bytes.Buffer
	cmd.SetErr(&errb)
	if b, err := readBundle(cmd, nil); err != nil || string(b) != testBundle || !strings.Contains(errb.String(), i18n.T("migrate.paste")) {
		t.Fatalf("%q %v\n%s", b, err, errb.String())
	}
	if got := typedNext(t, master, tty); got != "typed\n" {
		t.Fatalf("read %q", got)
	}
}

// typeIn writes s to the terminal through its master and waits until tty holds all of it: a pty moves what
// its master writes on its own time.
func typeIn(t *testing.T, master, tty *os.File, s string) {
	t.Helper()
	if _, err := master.WriteString(s); err != nil {
		t.Fatal(err)
	}
	for i := 0; queued(t, tty) < len(s); i++ {
		if i == 500 {
			t.Fatalf("terminal holds %d of %d bytes", queued(t, tty), len(s))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// typedNext types a line and reads the terminal's next line: the typed one when nothing was left before it.
func typedNext(t *testing.T, master, tty *os.File) string {
	t.Helper()
	typeIn(t, master, tty, "typed\n")
	b := make([]byte, 64)
	n, err := tty.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:n])
}

func TestImportSaveFailureShowsBundle(t *testing.T) {
	home := env(t, nil) // the daemon is down
	answer(t, testCode)
	os.WriteFile(filepath.Join(home, "migrate"), nil, 0o600) // no dir can be made there
	stdout, stderr, err := runSplit(t, testBundle, "migrate", "import")
	merr := os.MkdirAll(filepath.Join(home, "migrate"), 0o700) // the same refusal
	if exitCode(err) != 1 || merr == nil || stdout != testBundle || !strings.Contains(stderr, i18n.T("migrate.save_failed", merr)) {
		t.Fatalf("%v %v\nstdout %q\nstderr %s", err, merr, stdout, stderr)
	}
}

func TestImportStdinSurvivesSignals(t *testing.T) {
	f := &fake{importErrs: []error{migrate.ErrBadCode}}
	f.onImport = func() { interruptSelf(t) } // without a handler the test binary dies here
	home := env(t, f)
	answer(t, testCode)
	if _, _, err := runSplit(t, testBundle, "migrate", "import", "--code", testCode); exitCode(err) != 1 {
		t.Fatal(err)
	}
	saved(t, home, "received", testBundle)
}

func TestImportFileNotDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the read-only dir this test needs")
	}
	f := &fake{}
	home := env(t, f)
	dir := filepath.Join(home, "ro")
	os.Mkdir(dir, 0o700)
	file := filepath.Join(dir, "in.ccswap")
	os.WriteFile(file, []byte(testBundle), 0o600)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	answer(t, testCode)
	_, stderr, err := runSplit(t, "", "migrate", "import", file)
	rerr := os.Remove(file) // the same refusal
	if err != nil || rerr == nil || !strings.Contains(stderr, i18n.T("migrate.bundle_not_deleted", file, rerr)) {
		t.Fatalf("%v %v\n%s", err, rerr, stderr)
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	for _, c := range []struct{ in, err string }{
		{"hello\n", i18n.T("daemon.not_bundle")},
		{"", i18n.T("daemon.not_bundle")},
		{strings.TrimSuffix(testBundle, "-----END CCSWAP BUNDLE-----\n"), i18n.T("daemon.not_bundle")}, // cut short
	} {
		asked := answer(t, testCode)
		_, _, err := runSplit(t, c.in, "migrate", "import")
		if err == nil || err.Error() != c.err || *asked != 0 {
			t.Fatalf("%q: %v, asked %d", c.in, err, *asked)
		}
	}
	if f.migrateCalls() != "" {
		t.Fatalf("calls %q", f.migrateCalls())
	}
	noneSaved(t, home)
}

func TestImportTooLarge(t *testing.T) {
	f := &fake{}
	home := env(t, f)
	big := strings.Repeat("A", 1<<20+1)
	file := filepath.Join(home, "big.ccswap")
	os.WriteFile(file, []byte(big), 0o600)
	old := isTerminal
	t.Cleanup(func() { isTerminal = old })
	for _, c := range []struct {
		args  []string
		paste bool
	}{{nil, false}, {nil, true}, {[]string{file}, false}} {
		isTerminal = func(v any) bool { _, ok := v.(*strings.Reader); return ok && c.paste }
		_, _, err := runSplit(t, big, append([]string{"migrate", "import"}, c.args...)...)
		if !errors.Is(err, errTooLarge) {
			t.Fatalf("%v paste %v: %v", c.args, c.paste, err)
		}
	}
	if f.migrateCalls() != "" {
		t.Fatalf("calls %q", f.migrateCalls())
	}
	noneSaved(t, home)
}

// TestExportRefusals: a daemon refusal reaches the user in the daemon's words, which name the account, with no
// file written, no code shown and no other call made. Through the real server, it pins every refusal code
// export can get to migrateErr's list.
func TestExportRefusals(t *testing.T) {
	for _, refusal := range []error{
		i18n.Err("daemon.migrate_active", daemon.ErrMigrateActive, "a"),
		i18n.Err("daemon.migrate_not_found", daemon.ErrNotFound, "a"),
		i18n.Err("daemon.migrate_needs_login", switcher.ErrNeedsLogin, "a"),
		i18n.Err("daemon.migrating", switcher.ErrFrozen, "a"),
		i18n.Err("daemon.migrate_refreshing", switcher.ErrTargetRefreshing, "a"),
		i18n.Err("daemon.migrate_invalid_name", daemon.ErrInvalidName, "a"),
	} {
		f := exportFake()
		f.exportErr = refusal
		home := env(t, f)
		file := filepath.Join(home, "out.ccswap")
		stdout, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
		if err == nil || err.Error() != refusal.Error() || exitCode(err) != -1 {
			t.Fatalf("%v: %v", refusal, err)
		}
		if _, serr := os.Stat(file); !os.IsNotExist(serr) || strings.Contains(stdout+stderr, testCode) || f.migrateCalls() != "export" {
			t.Fatalf("%v: %v %q\n%s", refusal, serr, f.migrateCalls(), stderr)
		}
	}
}

// TestImportRefusals: the same for import, whose bundle file stays.
func TestImportRefusals(t *testing.T) {
	for _, refusal := range []error{
		i18n.Err("daemon.migrate_email_here", store.ErrDuplicate, "a@x.y", "a"),
		i18n.Err("daemon.migrate_name_taken", store.ErrDuplicate, "a", "z@x.y"),
		i18n.Err("daemon.migrate_active", daemon.ErrMigrateActive, "a"),
		i18n.Err("daemon.migrating", switcher.ErrFrozen, "a"),
		i18n.Err("daemon.migrate_refreshing", switcher.ErrTargetRefreshing, "a"),
		i18n.Err("daemon.migrate_spent", daemon.ErrBundleSpent, "a", "a"),
		i18n.Err("daemon.migrate_unchecked", daemon.ErrBundleUnchecked, "a"),
		i18n.Err("daemon.migrate_unchecked_late", daemon.ErrBundleUnchecked, "a"),
	} {
		f := &fake{importErrs: []error{refusal}}
		home := env(t, f)
		file := writeBundle(t, home)
		stdout, _, err := runSplit(t, "", "migrate", "import", file, "--code", testCode, "--replace")
		if err == nil || err.Error() != refusal.Error() || exitCode(err) != -1 || stdout != "" || f.migrateCalls() != "import" {
			t.Fatalf("%v: %v %q", refusal, err, f.migrateCalls())
		}
		if b, _ := os.ReadFile(file); string(b) != testBundle {
			t.Fatalf("%v: file %q", refusal, b)
		}
		noneSaved(t, home)
	}
}

// TestMigrateErrLanguage: a refusal that names no account is told in the terminal's language, not in the
// daemon's, which can differ; one that names accounts keeps the daemon's words.
func TestMigrateErrLanguage(t *testing.T) {
	for _, c := range []string{"not_bundle", "bundle_version", "invalid_code", "bad_bundle", "export_gone", "export_done"} {
		if err := migrateErr(409, fmt.Appendf(nil, `{"code":%q,"message":"in the daemon's language"}`, c)); err.Error() != i18n.T("daemon."+c) {
			t.Errorf("%s: %v", c, err)
		}
	}
	if err := migrateErr(409, []byte(`{"code":"migrate_active","message":"a, in the daemon's language"}`)); err.Error() != "a, in the daemon's language" {
		t.Error(err)
	}
}

// TestExportSlowDaemon: an export the daemon does not answer in time may have packed and frozen the accounts:
// the user is told nothing left and until when they stay frozen.
func TestExportSlowDaemon(t *testing.T) {
	f := exportFake()
	home := env(t, f)
	old := writeTimeout
	writeTimeout = 200 * time.Millisecond
	f.blockExport = make(chan struct{})
	t.Cleanup(func() { close(f.blockExport); writeTimeout = old }) // before env's cleanup stops the server
	file := filepath.Join(home, "out.ccswap")
	before := time.Now()
	_, stderr, err := runSplit(t, "", "migrate", "export", "a", "--yes", "-o", file)
	until := func(t0 time.Time) string { return i18n.T("migrate.export_slow", t0.Add(6*time.Minute).Format("15:04")) }
	if err == nil || err.Error() != until(before) && err.Error() != until(time.Now()) || f.migrateCalls() != "export" {
		t.Fatalf("%v %q\n%s", err, f.migrateCalls(), stderr)
	}
	if _, serr := os.Stat(file); !os.IsNotExist(serr) || strings.Contains(stderr, testCode) {
		t.Fatal("a file or a code without a bundle")
	}
}
