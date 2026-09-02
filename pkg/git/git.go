package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/adoramshoval/albs/pkg/interfaces"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Cloner clones repositories in-process via go-git, with no dependency on a
// git binary being present on PATH.
type Cloner struct {
	log interfaces.Logger
}

func NewCloner(log interfaces.Logger) interfaces.GitCloner {
	return &Cloner{log: log}
}

func (c *Cloner) warnf(format string, a ...interface{}) {
	if c.log != nil {
		c.log.Warnf(format, a...)
	}
}

// ResolveRef maps a version onto a fully qualified ref by listing the remote,
// rather than guessing at a naming convention.
//
// Paketo pins dependencies by OCI image tag ("cpython:1.18.42") while the
// corresponding Git repository tags the same release "v1.18.42", so an exact
// match is not enough.
func (c *Cloner) ResolveRef(ctx context.Context, repoURL, version string) (string, error) {
	if version == "" {
		return "", nil
	}

	refs, err := listRefs(ctx, repoURL)
	if err != nil {
		return "", fmt.Errorf("listing refs for %s: %w", repoURL, err)
	}

	var tags []string
	for _, ref := range refs {
		name := ref.Name()
		if !name.IsTag() && !name.IsBranch() {
			continue
		}
		if name.Short() == version {
			return name.String(), nil
		}
		if name.IsTag() {
			tags = append(tags, name.Short())
		}
	}

	want := normalize(version)
	for _, ref := range refs {
		if ref.Name().IsTag() && normalize(ref.Name().Short()) == want {
			return ref.Name().String(), nil
		}
	}

	if isCommitish(version) {
		return version, nil
	}

	return "", fmt.Errorf("no tag or branch matching %q in %s; available tags: %s",
		version, repoURL, summarize(tags))
}

func (c *Cloner) Clone(ctx context.Context, repoURL, ref, targetDir string) error {
	opts := &gogit.CloneOptions{
		URL:          repoURL,
		Depth:        1,
		SingleBranch: true,
		Tags:         gogit.NoTags,
	}

	// A bare commit SHA cannot be named in a shallow clone, so fall back to a
	// full clone plus checkout for that case only.
	var commit string
	if ref != "" {
		if strings.HasPrefix(ref, "refs/") {
			opts.ReferenceName = plumbing.ReferenceName(ref)
		} else {
			commit = ref
			opts.Depth = 0
			opts.SingleBranch = false
			opts.Tags = gogit.AllTags
		}
	}

	// Checkout goes through a filesystem that tolerates colliding paths.
	// Several Paketo components vendor ncurses terminfo fixtures whose paths
	// differ only in case; where the filesystem cannot distinguish them the
	// git CLI warns and carries on, whereas go-git fails the clone outright.
	worktree := osfs.New(targetDir)
	dotGit, err := worktree.Chroot(gogit.GitDirName)
	if err != nil {
		return fmt.Errorf("preparing clone of %s: %w", repoURL, err)
	}
	storer := filesystem.NewStorage(dotGit, cache.NewObjectLRUDefault())

	var collisions int
	tolerant := collisionTolerantFS{
		Filesystem:  worktree,
		onCollision: func(string) { collisions++ },
	}

	repo, err := gogit.CloneContext(ctx, storer, tolerant, opts)
	if err != nil {
		return fmt.Errorf("cloning %s at %s: %w", repoURL, display(ref), err)
	}
	if collisions > 0 {
		c.warnf("%s: skipped %d colliding path(s) during checkout", repoURL, collisions)
	}

	if commit == "" {
		return nil
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("opening worktree for %s: %w", repoURL, err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(commit)}); err != nil {
		return fmt.Errorf("checking out %s in %s: %w", commit, repoURL, err)
	}
	return nil
}

// collisionTolerantFS swallows the "file exists" error raised when checking
// out a symlink whose path is already taken, mirroring the git CLI: report the
// collision and keep the checkout usable rather than abandoning it.
//
// In practice this only fires where the filesystem cannot distinguish two
// paths a Git tree treats as distinct -- case-insensitive volumes on macOS and
// Windows, or Unicode-normalising ones. On a case-sensitive filesystem a valid
// tree cannot produce the collision, so this is inert rather than
// platform-specific behaviour.
type collisionTolerantFS struct {
	billy.Filesystem
	onCollision func(path string)
}

func (fs collisionTolerantFS) Symlink(target, link string) error {
	err := fs.Filesystem.Symlink(target, link)
	if err != nil && errors.Is(err, os.ErrExist) {
		fs.onCollision(link)
		return nil
	}
	return err
}

func (fs collisionTolerantFS) Chroot(path string) (billy.Filesystem, error) {
	inner, err := fs.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return collisionTolerantFS{Filesystem: inner, onCollision: fs.onCollision}, nil
}

func listRefs(ctx context.Context, repoURL string) ([]*plumbing.Reference, error) {
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})
	return remote.ListContext(ctx, &gogit.ListOptions{})
}

// normalize strips the leading "v" that Git tags carry and image tags do not.
func normalize(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func isCommitish(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func display(ref string) string {
	if ref == "" {
		return "default branch"
	}
	return ref
}

// summarize keeps the error readable for repositories with hundreds of tags.
func summarize(tags []string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	sort.Sort(sort.Reverse(sort.StringSlice(tags)))
	const max = 10
	if len(tags) > max {
		return strings.Join(tags[:max], ", ") + fmt.Sprintf(" (and %d more)", len(tags)-max)
	}
	return strings.Join(tags, ", ")
}
