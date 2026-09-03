// Package fsutil moves archive files around without copying their bytes when
// the filesystem allows it.
//
// albs deals in offline component archives, which carry every dependency a
// buildpack declares and run to several gigabytes each. Materialising one twice
// is measurable; materialising one three times, as albs did when it copied jam's
// output into the cache and then copied the cache entry into the workspace, is
// most of the tool's disk traffic.
package fsutil

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// bufSize matches pkg/multiarch: large enough to make the syscall count
// irrelevant, small enough that holding one per concurrent worker is free.
const bufSize = 64 << 10

// LinkOrCopy makes dst refer to src's contents, hardlinking when src and dst
// live on the same filesystem and streaming a copy when they do not.
//
// A hardlink means dst and src share an inode, so a write through either is
// visible through both. Every caller here treats archives as immutable, and
// DiskCache marks its entries read-only to keep that honest.
func LinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	return Copy(src, dst)
}

// Copy streams src to dst through a fixed buffer, never holding the file in
// memory. dst is removed if the copy fails, so a partial archive is never left
// where a later run could mistake it for a complete one.
func Copy(src, dst string) (err error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dstFile.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()

	bw := bufio.NewWriterSize(dstFile, bufSize)
	if _, err = io.CopyBuffer(bw, bufio.NewReaderSize(srcFile, bufSize), make([]byte, bufSize)); err != nil {
		return err
	}
	return bw.Flush()
}

// isCrossDevice reports whether a link failed because the two paths are on
// different filesystems, or because the filesystem has no hardlinks at all.
//
// Both are ordinary conditions rather than faults: /tmp is frequently a
// separate mount, and tmpfs, FAT and many network filesystems refuse links
// outright. Anything else -- a missing source, a name already taken, a
// permission problem -- is a real error and must not be papered over by
// silently copying instead.
func isCrossDevice(err error) bool {
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	return errors.Is(linkErr.Err, syscall.EXDEV) ||
		errors.Is(linkErr.Err, syscall.ENOTSUP) ||
		errors.Is(linkErr.Err, syscall.EPERM) ||
		errors.Is(linkErr.Err, fs.ErrPermission)
}
