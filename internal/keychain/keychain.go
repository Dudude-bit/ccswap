// Package keychain wraps the macOS keychain via /usr/bin/security.
package keychain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("keychain item not found")
	ErrVerifyFailed = errors.New("keychain read-back does not match written value")
)

// security -i reads lines of ~4096 chars; longer commands get split mid-hex.
const maxStdinLine = 4000

type Store interface {
	Get(service, account string) ([]byte, error)
	Set(service, account string, secret []byte) error
	Delete(service, account string) error
}

// Security talks to /usr/bin/security. Runner is the test seam.
type Security struct {
	Runner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
}

func NewSecurity() *Security { return &Security{Runner: runSecurity} }

func runSecurity(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			// never include args/stdin: they may carry the secret.
			err = fmt.Errorf("security %s: %w: %s", args[0], err, msg)
		}
	}
	return out, err
}

// exit status 44 = errSecItemNotFound.
func mapErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 44 {
		return ErrNotFound
	}
	return err
}

func (s *Security) Get(service, account string) ([]byte, error) {
	out, err := s.Runner(context.Background(), nil, "find-generic-password", "-a", account, "-s", service, "-w")
	if err != nil {
		return nil, mapErr(err)
	}
	return bytes.TrimSuffix(out, []byte("\n")), nil
}

func (s *Security) Set(service, account string, secret []byte) error {
	h := hex.EncodeToString(secret)
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", quote(account), quote(service), h)
	var err error
	if len(line) <= maxStdinLine {
		_, err = s.Runner(context.Background(), []byte(line), "-i")
	} else {
		// ponytail: secret visible in argv for large blobs, same as Claude Code itself.
		_, err = s.Runner(context.Background(), nil, "add-generic-password", "-U", "-a", account, "-s", service, "-X", h)
	}
	if err != nil {
		return mapErr(err)
	}
	got, err := s.Get(service, account)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, secret) {
		return ErrVerifyFailed
	}
	return nil
}

func (s *Security) Delete(service, account string) error {
	_, err := s.Runner(context.Background(), nil, "delete-generic-password", "-a", account, "-s", service)
	return mapErr(err)
}

func quote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
