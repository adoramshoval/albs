package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// Put writes via a temporary file and renames into place. rename is atomic on
// POSIX, so concurrent writers of the same key cannot observe a torn archive
// and an interrupted run cannot leave a truncated entry behind that later runs
// would trust.
func (c *DiskCache) Put(key, srcFilePath string) (err error) {
	destPath := c.pathFor(key)

	srcFile, err := os.Open(srcFilePath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	tmpFile, err := os.CreateTemp(c.baseDir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if _, err = io.Copy(tmpFile, srcFile); err != nil {
		tmpFile.Close()
		return err
	}
	if err = tmpFile.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, destPath)
}

func (c *DiskCache) pathFor(key string) string {
	return filepath.Join(c.baseDir, c.hashKey(key)+".tgz")
}

func (c *DiskCache) hashKey(key string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", c.salt, key)
	return hex.EncodeToString(h.Sum(nil))
}
