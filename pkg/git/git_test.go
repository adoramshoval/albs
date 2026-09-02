package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// newRepo builds a real on-disk repository carrying the given tags, so ref
// resolution is exercised end to end without touching the network.
func newRepo(t *testing.T, tags ...string) string {
	t.Helper()

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "buildpack.toml"), []byte("api = \"0.7\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("buildpack.toml"); err != nil {
		t.Fatalf("add: %v", err)
	}
	hash, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, tag := range tags {
		if _, err := repo.CreateTag(tag, hash, nil); err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
	}
	return dir
}

func TestResolveRef(t *testing.T) {
	repo := newRepo(t, "v1.18.42", "v1.9.0", "0.4.0")
	cloner := &Cloner{}

	tests := []struct {
		name    string
		version string
		want    string
		wantErr string
	}{
		{
			// The case that broke every component build: Paketo pins the image
			// tag 1.18.42 while the Git tag is v1.18.42.
			name:    "image tag without the v prefix",
			version: "1.18.42",
			want:    "refs/tags/v1.18.42",
		},
		{
			name:    "exact tag match",
			version: "v1.9.0",
			want:    "refs/tags/v1.9.0",
		},
		{
			name:    "tag published without a v prefix",
			version: "0.4.0",
			want:    "refs/tags/0.4.0",
		},
		{
			name:    "empty version means the default branch",
			version: "",
			want:    "",
		},
		{
			name:    "unknown version reports the available tags",
			version: "9.9.9",
			wantErr: "no tag or branch matching",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cloner.ResolveRef(context.Background(), repo, tt.version)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolveRef(%q) = %q, want error", tt.version, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				// The error must name the alternatives, or it is no better
				// than the opaque clone failure it replaced.
				if !strings.Contains(err.Error(), "v1.18.42") {
					t.Errorf("error %q does not list available tags", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRef(%q): unexpected error: %v", tt.version, err)
			}
			if got != tt.want {
				t.Errorf("ResolveRef(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestResolveRefAcceptsCommitSHA(t *testing.T) {
	repo := newRepo(t, "v1.0.0")

	r, err := gogit.PlainOpen(repo)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	sha := head.Hash().String()

	got, err := (&Cloner{}).ResolveRef(context.Background(), repo, sha)
	if err != nil {
		t.Fatalf("ResolveRef(%q): unexpected error: %v", sha, err)
	}
	if got != sha {
		t.Errorf("ResolveRef(%q) = %q, want the SHA unchanged", sha, got)
	}
}

func TestCloneAtResolvedRef(t *testing.T) {
	repo := newRepo(t, "v1.18.42")
	cloner := &Cloner{}
	ctx := context.Background()

	ref, err := cloner.ResolveRef(ctx, repo, "1.18.42")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "checkout")
	if err := cloner.Clone(ctx, repo, ref, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "buildpack.toml")); err != nil {
		t.Fatalf("expected buildpack.toml in the clone: %v", err)
	}
}

func TestCloneReportsBadRef(t *testing.T) {
	repo := newRepo(t, "v1.0.0")
	dest := filepath.Join(t.TempDir(), "checkout")

	err := (&Cloner{}).Clone(context.Background(), repo, plumbing.NewTagReferenceName("nope").String(), dest)
	if err == nil {
		t.Fatal("expected an error cloning a nonexistent ref")
	}
	if !strings.Contains(err.Error(), repo) {
		t.Errorf("error %q does not name the repository", err)
	}
}

func TestNormalize(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"1.18.42", "1.18.42"},
		{"v1.18.42", "1.18.42"},
		{"  v2.0.0 ", "2.0.0"},
		{"", ""},
	} {
		if got := normalize(tt.in); got != tt.want {
			t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsCommitish(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"39c6dad2b4e70e6defc612d4ad91e63da644f786", true},
		{"39c6dad", true},
		{"39c6da", false},  // too short to be unambiguous
		{"1.18.42", false}, // a version, not a hash
		{"v1.18.42", false},
		{"zzzzzzz", false},
	} {
		if got := isCommitish(tt.in); got != tt.want {
			t.Errorf("isCommitish(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSummarizeTruncates(t *testing.T) {
	var tags []string
	for i := 0; i < 25; i++ {
		tags = append(tags, "v1.0."+string(rune('a'+i)))
	}
	got := summarize(tags)
	if !strings.Contains(got, "and 15 more") {
		t.Errorf("summarize did not truncate: %q", got)
	}
	if summarize(nil) != "(none)" {
		t.Errorf("summarize(nil) = %q, want (none)", summarize(nil))
	}
}

// Several Paketo components vendor ncurses terminfo fixtures whose paths
// differ only in case. On a case-insensitive filesystem the second symlink
// collides with the first; the git CLI warns and keeps going, and so must we.
func TestCollisionTolerantFSSwallowsSymlinkCollisions(t *testing.T) {
	dir := t.TempDir()

	var collided []string
	fs := collisionTolerantFS{
		Filesystem:  osfs.New(dir),
		onCollision: func(path string) { collided = append(collided, path) },
	}

	if err := fs.Symlink("../target", "link"); err != nil {
		t.Fatalf("first symlink: %v", err)
	}
	if len(collided) != 0 {
		t.Fatalf("unexpected collision on the first symlink: %v", collided)
	}

	if err := fs.Symlink("../other", "link"); err != nil {
		t.Fatalf("colliding symlink should be tolerated, got: %v", err)
	}
	if len(collided) != 1 || collided[0] != "link" {
		t.Errorf("collisions = %v, want exactly [link]", collided)
	}

	// The first link must survive, matching git's behaviour of keeping one.
	target, err := os.Readlink(filepath.Join(dir, "link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "../target" {
		t.Errorf("link points at %q, want %q", target, "../target")
	}
}

// erroringFS fails every Symlink with a fixed error.
type erroringFS struct {
	billy.Filesystem
	err error
}

func (f erroringFS) Symlink(string, string) error { return f.err }

// Only collisions are swallowed; any other failure must still propagate.
func TestCollisionTolerantFSPropagatesRealErrors(t *testing.T) {
	sentinel := errors.New("disk on fire")
	fs := collisionTolerantFS{
		Filesystem:  erroringFS{Filesystem: osfs.New(t.TempDir()), err: sentinel},
		onCollision: func(string) { t.Error("a real error is not a collision") },
	}

	if err := fs.Symlink("target", "link"); !errors.Is(err, sentinel) {
		t.Errorf("Symlink error = %v, want %v", err, sentinel)
	}
}

// Chroot must keep the tolerance, since checkout descends into subtrees.
func TestCollisionTolerantFSChrootStaysTolerant(t *testing.T) {
	var collided int
	fs := collisionTolerantFS{
		Filesystem:  osfs.New(t.TempDir()),
		onCollision: func(string) { collided++ },
	}

	sub, err := fs.Chroot("nested")
	if err != nil {
		t.Fatalf("Chroot: %v", err)
	}
	if _, ok := sub.(collisionTolerantFS); !ok {
		t.Fatalf("Chroot returned %T, want collisionTolerantFS", sub)
	}
	if err := sub.Symlink("../a", "link"); err != nil {
		t.Fatalf("first symlink: %v", err)
	}
	if err := sub.Symlink("../b", "link"); err != nil {
		t.Fatalf("colliding symlink in a chroot should be tolerated: %v", err)
	}
	if collided != 1 {
		t.Errorf("collisions = %d, want 1", collided)
	}
}
