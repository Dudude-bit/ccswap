package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dudude-bit/ccswap/internal/i18n"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		release, current string
		want             bool
	}{
		{"v0.2.0", "v0.1.1", true},
		{"v0.1.10", "v0.1.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.1.1", "v0.1.1", false},
		{"v0.1.1", "v0.2.0", false},
		{"v0.1.1", "v0.1.1+dirty", false},
		{"v0.2.0", "v0.2.0-rc.1", true},
		{"v0.2.0-rc.2", "v0.2.0-rc.1", true},
		{"v0.2.0-rc.10", "v0.2.0-rc.9", true},
		{"v0.2.0-rc.1", "v0.2.0-1", true},                         // a word after a number
		{"v0.2.0-rc.1.1", "v0.2.0-rc.1", true},                    // more fields
		{"v0.1.1", "v0.1.2-0.20261023061355-3b0e643abcde", false}, // go install …@commit past v0.1.1
		{"v0.1.2", "v0.1.2-0.20261023061355-3b0e643abcde", true},
		{"v0.1.1", "v0.1.1-5-g90b77e9", false}, // make build, five commits past v0.1.1
		{"v0.1.1", "v0.1.1-5-g90b77e9-dirty", false},
		{"v0.1.1", "v0.1.1-dirty", false},
		{"v0.1.2", "v0.1.1-5-g90b77e9", true},
		{"v0.2.0", "dev", false},
		{"v0.2.0", "(devel)", false},
		{"v0.2.0", "90b77e9", false}, // git describe --always without tags
		{"latest", "v0.1.1", false},
		{"v0.2.0", "v99999999999999999999.0.0", false},
	} {
		if got := Newer(c.release, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.release, c.current, got)
		}
	}
}

// release is a fake GitHub with one release of tag, whose archive for this platform holds a ccswap that
// prints says to --version; corrupt breaks the archive's checksum.
type release struct {
	tag, says string
	corrupt   bool
	noBuild   bool
	asks      int
}

func (r *release) serve(t *testing.T) {
	t.Helper()
	exe := []byte("#!/bin/sh\necho 'ccswap version " + r.says + "'\n")
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("MIT")}, {"ccswap", exe}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.body)
	}
	tw.Close()
	zw.Close()
	name := Release{Version: r.tag}.archive()
	sum := sha256.Sum256(tgz.Bytes())
	if r.corrupt {
		sum[0] ^= 1
	}
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  ccswap_other.tar.gz\n"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/Dudude-bit/ccswap/releases/latest":
			r.asks++
			if req.Header.Get("User-Agent") != "ccswap/test" {
				t.Errorf("user agent %q", req.Header.Get("User-Agent"))
			}
			assets := []map[string]string{{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"}}
			if !r.noBuild {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": srv.URL + "/tgz"})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": r.tag, "html_url": "https://evil.example/", "assets": assets})
		case "/sums":
			w.Write([]byte(sums))
		case "/tgz":
			w.Write(tgz.Bytes())
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })
}

// installed is a ccswap binary in a temp folder, and its content.
func installed(t *testing.T) (string, []byte) {
	bin := filepath.Join(t.TempDir(), "ccswap")
	old := []byte("#!/bin/sh\necho 'ccswap version v0.1.1'\n")
	if err := os.WriteFile(bin, old, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, old
}

func TestLatestAndInstall(t *testing.T) {
	r := &release{tag: "v9.9.9", says: "v9.9.9"}
	r.serve(t)
	rel, err := Latest(context.Background(), "ccswap/test")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v9.9.9" || rel.URL != "https://github.com/Dudude-bit/ccswap/releases/tag/v9.9.9" {
		t.Fatalf("%+v", rel) // the page links to URL: never the address GitHub's answer gives
	}
	bin, _ := installed(t)
	if err := Install(context.Background(), rel, bin); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin).Output()
	if err != nil || strings.TrimSpace(string(out)) != "ccswap version v9.9.9" {
		t.Fatalf("%v %q", err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".ccswap-update-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestInstallRefusals(t *testing.T) {
	name := Release{Version: "v9.9.9"}.archive()
	for _, c := range []struct {
		name string
		r    release
		want string
	}{
		{"checksum", release{tag: "v9.9.9", says: "v9.9.9", corrupt: true}, i18n.In(i18n.EN, "update.checksum_mismatch", name)},
		{"other version", release{tag: "v9.9.9", says: "v9.9.8"}, i18n.In(i18n.EN, "update.wrong_version", "ccswap version v9.9.8", "v9.9.9")},
		{"no build", release{tag: "v9.9.9", says: "v9.9.9", noBuild: true}, i18n.In(i18n.EN, "update.no_build", "v9.9.9", runtime.GOOS+"/"+runtime.GOARCH)},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.r.serve(t)
			rel, err := Latest(context.Background(), "ccswap/test")
			if err != nil {
				t.Fatal(err)
			}
			bin, old := installed(t)
			err = Install(context.Background(), rel, bin)
			var ie *i18n.Error
			if !errors.As(err, &ie) || ie.In(i18n.EN) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if b, _ := os.ReadFile(bin); !bytes.Equal(b, old) {
				t.Fatal("the binary changed")
			}
			if left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".ccswap-update-*")); len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
		})
	}
}

func TestLatestRefusesOddTag(t *testing.T) {
	r := &release{tag: "v1.0.0\"><script>", says: "x"}
	r.serve(t)
	if _, err := Latest(context.Background(), "ccswap/test"); err == nil {
		t.Fatal("an odd tag was taken")
	}
}

func TestBinaryRefusesOversize(t *testing.T) {
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	// only the header: the size it declares is refused before a byte of the file is read
	if err := tw.WriteHeader(&tar.Header{Name: "ccswap", Mode: 0o755, Size: maxBinary + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Flush()
	zw.Close()
	exe, err := binary(tgz.Bytes())
	var ie *i18n.Error
	if exe != nil || !errors.As(err, &ie) || ie.In(i18n.EN) != i18n.In(i18n.EN, "update.too_big", "ccswap") {
		t.Fatalf("%d bytes, %v", len(exe), err)
	}
}
