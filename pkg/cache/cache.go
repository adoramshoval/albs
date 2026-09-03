package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/adoramshoval/albs/pkg/fsutil"
	"github.com/adoramshoval/albs/pkg/interfaces"
)

// DiskCache stores built component archives keyed by content-independent
// identity (source repo, ref and packer version).
type DiskCache struct {
	baseDir string
	// salt distinguishes archives produced by different packer versions, so
	// that upgrading jam does not silently reuse incompatible entries.
	salt string
}

func NewDiskCache(baseDir, salt string) (interfaces.CacheManager, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}
	return &DiskCache{baseDir: baseDir, salt: salt}, nil
}

func (c *DiskCache) Get(key string) (string, bool) {
	cachedPath := c.pathFor(key)
	if info, err := os.Stat(cachedPath); err == nil && info.Size() > 0 {
		return cachedPath, true
	}
	return "", false
}

// entryMode marks cache entries read-only.
//
// Entries are hardlinked rather than copied wherever the filesystem allows, so
// a cached archive, jam's output it was linked from, and the vendored copy pack
// reads can all be the same inode. Nothing in albs rewrites an archive in
// place, and this makes the filesystem enforce that rather than leaving it as
// an invariant someone has to remember.
const entryMode fs.FileMode = 0o444

// Put publishes srcFilePath under key, hardlinking it into the cache when the
// filesystem allows and copying it when it does not.
//
// Both paths publish atomically. A hardlink is created fully formed under its
// final name, so there is no intermediate state to observe; a copy is written
// to a temporary file and renamed, rename being atomic on POSIX. Either way a
// concurrent reader never sees a torn archive, and an interrupted run leaves
// nothing truncated behind for a later run to trust.
func (c *DiskCache) Put(key, srcFilePath string) (err error) {
	destPath := c.pathFor(key)

	// Linking costs the same whether the archive is a kilobyte or five
	// gigabytes, which for offline component archives is the difference that
	// matters.
	if err := os.Link(srcFilePath, destPath); err == nil {
		return os.Chmod(destPath, entryMode)
	} else if errors.Is(err, fs.ErrExist) {
		// Another worker published this key first. Its entry is as good as
		// ours, since the key already pins the source ref and packer version.
		return nil
	}

	tmpFile, err := os.CreateTemp(c.baseDir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if err = tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if err = fsutil.Copy(srcFilePath, tmpPath); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, entryMode); err != nil {
		return err
	}
	return os.Rename(tmpPath, destPath)
}

// GetMeta returns the metadata stored beside key's archive.
//
// A miss is ordinary rather than exceptional: entries cached before metadata
// was recorded have none, and the caller falls back to what the archive itself
// can tell it.
func (c *DiskCache) GetMeta(key string) ([]byte, bool) {
	data, err := os.ReadFile(c.metaPathFor(key))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// PutMeta stores metadata beside key's archive.
//
// Written to a temporary file and renamed, so a reader never sees a partial
// blob and an interrupted run leaves nothing truncated for a later one to
// trust -- the same guarantee Put gives the archive.
func (c *DiskCache) PutMeta(key string, data []byte) (err error) {
	tmpFile, err := os.CreateTemp(c.baseDir, ".tmpmeta-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if _, err = tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err = tmpFile.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, entryMode); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.metaPathFor(key))
}

func (c *DiskCache) pathFor(key string) string {
	return filepath.Join(c.baseDir, c.hashKey(key)+".tgz")
}

// metaPathFor sits beside pathFor under the same hash, so an archive and its
// metadata share a name and are obvious as a pair in a directory listing.
func (c *DiskCache) metaPathFor(key string) string {
	return filepath.Join(c.baseDir, c.hashKey(key)+".buildpack.toml")
}

func (c *DiskCache) hashKey(key string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", c.salt, key)
	return hex.EncodeToString(h.Sum(nil))
}
