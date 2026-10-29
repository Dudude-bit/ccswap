// Package update finds ccswap's newest release on GitHub and installs it over a ccswap binary.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Dudude-bit/ccswap/internal/atomicfile"
	"github.com/Dudude-bit/ccswap/internal/i18n"
)

const repo = "Dudude-bit/ccswap"

// API is GitHub's REST API, a test seam.
var API = "https://api.github.com"

// No client timeout: a download takes as long as the caller's context allows.
var client = &http.Client{}

// Release is a published ccswap release.
type Release struct {
	Version string // its tag, such as v0.2.0
	URL     string // its page
	assets  map[string]string
}

// Latest asks GitHub for the newest release; drafts and prereleases are never the latest.
func Latest(ctx context.Context, userAgent string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, API+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, i18n.Err("update.no_answer", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, i18n.Err("update.github_status", resp.StatusCode)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, i18n.Err("update.unreadable", err)
	}
	if !Known(r.Tag) { // the tag goes into the release page's address and a file name
		return Release{}, i18n.Err("update.bad_tag", r.Tag)
	}
	rel := Release{Version: r.Tag, URL: "https://github.com/" + repo + "/releases/tag/" + r.Tag, assets: map[string]string{}}
	for _, a := range r.Assets {
		rel.assets[a.Name] = a.URL
	}
	return rel, nil
}

// archive is the name goreleaser gives rel's build for this platform.
func (rel Release) archive() string {
	return fmt.Sprintf("ccswap_%s_%s_%s.tar.gz", strings.TrimPrefix(rel.Version, "v"), runtime.GOOS, runtime.GOARCH)
}

const (
	maxArchive = 64 << 20
	maxBinary  = 128 << 20
)

// Install puts rel's build for this platform at bin. The archive must match the release's checksums.txt
// and its binary must run and report rel's version before one rename replaces bin, so a running ccswap
// keeps its old file and nothing ever runs half a binary.
func Install(ctx context.Context, rel Release, bin string) error {
	name := rel.archive()
	if rel.assets[name] == "" {
		return i18n.Err("update.no_build", rel.Version, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if rel.assets["checksums.txt"] == "" {
		return i18n.Err("update.no_checksums", rel.Version)
	}
	sums, err := get(ctx, rel.assets["checksums.txt"], 1<<16)
	if err != nil {
		return err
	}
	want, ok := checksum(sums, name)
	if !ok {
		return i18n.Err("update.no_checksum", name)
	}
	tgz, err := get(ctx, rel.assets[name], maxArchive)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(tgz); hex.EncodeToString(sum[:]) != want {
		return i18n.Err("update.checksum_mismatch", name)
	}
	exe, err := binary(tgz)
	if err != nil {
		return i18n.Err("update.bad_archive", name, err)
	}
	if exe == nil {
		return i18n.Err("update.not_in_archive", name)
	}
	return replace(ctx, bin, exe, rel.Version)
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, i18n.Err("update.no_answer", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, i18n.Err("update.github_status", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, i18n.Err("update.no_answer", err)
	}
	if int64(len(b)) > limit {
		return nil, i18n.Err("update.too_big", url)
	}
	return b, nil
}

// checksum is name's SHA-256 in a sha256sum listing.
func checksum(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// binary is the ccswap file at the top of a release archive; nil when it has none.
func binary(tgz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == "ccswap" {
			return io.ReadAll(io.LimitReader(tr, maxBinary))
		}
	}
}

// replace writes exe beside bin, runs it, and renames it over bin.
func replace(ctx context.Context, bin string, exe []byte, version string) error {
	f, err := os.CreateTemp(filepath.Dir(bin), ".ccswap-update-*")
	if err != nil {
		return i18n.Err("update.cannot_write", bin, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(exe)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil {
		err = atomicfile.Sync(f)
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return i18n.Err("update.cannot_write", bin, err)
	}
	if err := reports(ctx, tmp, version); err != nil {
		return err
	}
	if err := os.Rename(tmp, bin); err != nil {
		return i18n.Err("update.cannot_write", bin, err)
	}
	atomicfile.SyncDir(filepath.Dir(bin))
	return nil
}

// reports runs `exe --version` and checks that it names version: the build runs here, and it is the one
// the release says.
func reports(ctx context.Context, exe, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil {
		return i18n.Err("update.does_not_run", err)
	}
	if !slices.Contains(strings.Fields(string(out)), version) {
		return i18n.Err("update.wrong_version", strings.TrimSpace(string(out)), version)
	}
	return nil
}
