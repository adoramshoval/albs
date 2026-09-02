package packer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/adoramshoval/albs/pkg/interfaces"
)

// MinJamVersion is the lowest jam release albs is known to work against.
const MinJamVersion = "2.0.0"

// JamCLI shells out to the jam binary. jam's packaging logic lives in an
// internal/ package upstream and is therefore not importable, so this stays a
// subprocess; Preflight makes that dependency fail loudly and early.
type JamCLI struct {
	binary string
	log    interfaces.Logger

	once        sync.Once
	version     string
	fingerprint string
	resolveErr  error
}

// NewJamPacker returns the concrete type so callers can run Preflight before
// wiring it in as an interfaces.JamPacker.
func NewJamPacker(log interfaces.Logger) *JamCLI {
	return &JamCLI{binary: "jam", log: log}
}

var _ interfaces.JamPacker = (*JamCLI)(nil)

// Preflight verifies jam is installed and new enough before any cloning or
// packaging begins, so a missing dependency surfaces immediately rather than
// from inside a worker goroutine after minutes of work.
func (j *JamCLI) Preflight(ctx context.Context) error {
	if _, err := exec.LookPath(j.binary); err != nil {
		return fmt.Errorf("jam not found on PATH: install it with "+
			"`go install github.com/paketo-buildpacks/jam/v2@latest` and ensure "+
			"$(go env GOPATH)/bin is on your PATH: %w", err)
	}

	version, err := j.Version(ctx)
	if err != nil {
		return err
	}

	// Releases stamp their version via ldflags; binaries produced by
	// `go install` report an empty one. That is not a reason to refuse to run,
	// so the minimum-version check is skipped rather than failed.
	if version == "" {
		j.log.Warnf("jam reports no version (typical of `go install` builds); skipping the >= %s check", MinJamVersion)
		return nil
	}
	if !atLeast(version, MinJamVersion) {
		return fmt.Errorf("jam %s is too old: albs requires >= %s", version, MinJamVersion)
	}
	j.log.Debugf("using jam %s", version)
	return nil
}

// Version reports jam's self-declared version, which may legitimately be empty.
func (j *JamCLI) Version(ctx context.Context) (string, error) {
	j.resolve(ctx)
	return j.version, j.resolveErr
}

// Fingerprint identifies this jam build for cache-keying purposes. It prefers
// the declared version and falls back to hashing the binary, so that cache
// entries built by an unversioned `go install` jam are still invalidated when
// that binary changes.
func (j *JamCLI) Fingerprint(ctx context.Context) (string, error) {
	j.resolve(ctx)
	return j.fingerprint, j.resolveErr
}

func (j *JamCLI) resolve(ctx context.Context) {
	j.once.Do(func() {
		out, err := exec.CommandContext(ctx, j.binary, "version").CombinedOutput()
		if err != nil {
			j.resolveErr = fmt.Errorf("could not run `jam version` (%w): %s", err, strings.TrimSpace(string(out)))
			return
		}

		j.version = parseVersion(string(out))
		if j.version != "" {
			j.fingerprint = "jam " + j.version
			return
		}

		path, err := exec.LookPath(j.binary)
		if err != nil {
			j.resolveErr = err
			return
		}
		sum, err := hashFile(path)
		if err != nil {
			j.resolveErr = fmt.Errorf("fingerprinting jam binary: %w", err)
			return
		}
		j.fingerprint = "jam sha256:" + sum
	})
}

// PackOffline builds an offline archive of the buildpack in srcDir.
//
// version is required: Paketo component buildpack.toml files carry no version
// of their own, and the meta-buildpack's order groups pin exact versions, so a
// package built without it cannot satisfy the composite.
func (j *JamCLI) PackOffline(ctx context.Context, srcDir, version, outputTgzPath string) error {
	if version == "" {
		return fmt.Errorf("cannot pack %s: no version to stamp the buildpack with", srcDir)
	}

	cmd := exec.CommandContext(ctx, j.binary, "pack",
		"--buildpack", filepath.Join(srcDir, "buildpack.toml"),
		"--version", version,
		"--output", outputTgzPath,
		"--offline",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("jam pack failed (%w): %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseVersion pulls a dotted version out of jam's output, which prefixes it
// with the binary name.
func parseVersion(out string) string {
	for _, field := range strings.Fields(out) {
		field = strings.TrimPrefix(strings.TrimSpace(field), "v")
		if field == "" {
			continue
		}
		if field[0] >= '0' && field[0] <= '9' && strings.Contains(field, ".") {
			return field
		}
	}
	return ""
}

// atLeast compares dotted numeric versions field by field.
func atLeast(have, want string) bool {
	h, w := splitVersion(have), splitVersion(want)
	for i := 0; i < len(w); i++ {
		var hv int
		if i < len(h) {
			hv = h[i]
		}
		if hv != w[i] {
			return hv > w[i]
		}
	}
	return true
}

func splitVersion(v string) []int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}
