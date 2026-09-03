// Package multiarch flattens the per-platform directory layout that Paketo
// component buildpacks are published in.
//
// Component buildpacks are now built for several platforms at once, so their
// binaries land under <os>/<arch>/bin rather than bin/, and jam packs exactly
// that layout. The lifecycle only ever execs <buildpack root>/bin/detect, so
// the wanted platform has to be hoisted to the archive root before packaging.
//
// pack does this for the buildpack named by package.toml's [buildpack] uri
// (see buildpack.PlatformRootFolder) but not for the entries in
// [[dependencies]]: those are read verbatim by buildpack.FromBuildpackRootBlob.
// An unflattened component therefore packages without complaint and fails at
// build time with "fork/exec .../bin/detect: no such file or directory".
package multiarch

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/adoramshoval/albs/pkg/fsutil"
)

// bufSize is the buffer used for both file handles and the per-entry copy.
//
// gzip pushes ~246-byte writes at whatever it wraps (flate's bufferFlushSize)
// and reads through a 4KiB bufio of its own, so unbuffered file handles cost
// millions of syscalls on a multi-gigabyte archive. 64KiB removes over 99% of
// them; larger buffers chase the remaining fraction of a percent while scaling
// with --concurrency, and by then the cost is deflate itself.
const bufSize = 64 << 10

// compressionLevel is the deflate level used when rewriting an archive.
//
// An offline archive is almost entirely already-compressed dependency
// tarballs, so deflate bails out to stored blocks and the level scarcely
// engages: BenchmarkCompressionLevel measures BestSpeed at 3% faster than the
// default for a byte-identical output. Small, but free. NoCompression is
// faster still (16%) and barely larger here, yet that margin depends on the
// payload being incompressible -- it is not, for an archive packed without
// jam's --offline -- so it is not worth the coupling.
const compressionLevel = gzip.BestSpeed

// Target is the platform whose files are hoisted to the buildpack root.
type Target struct {
	OS   string
	Arch string
}

// ParseTarget reads an "<os>/<arch>" pair, the spelling used by both pack's
// --target and Paketo's scripts/build.sh.
func ParseTarget(s string) (Target, error) {
	goos, arch, found := strings.Cut(s, "/")
	if !found || goos == "" || arch == "" || strings.Contains(arch, "/") {
		return Target{}, fmt.Errorf("invalid target %q: want <os>/<arch>, for example linux/amd64", s)
	}
	return Target{OS: goos, Arch: arch}, nil
}

func (t Target) String() string { return t.OS + "/" + t.Arch }

// prefix is the directory a multi-arch archive keeps this platform's files in.
func (t Target) prefix() string { return t.OS + "/" + t.Arch + "/" }

// Flatten writes the gzipped tar at srcPath to dstPath with everything under
// <os>/<arch>/ moved to the archive root and the other platforms dropped.
//
// Archives that already carry bin/ at the root are passed through untouched --
// hardlinked to dstPath where the filesystem allows, copied where it does not --
// so this is safe to apply to every component regardless of how it was built.
func Flatten(srcPath, dstPath string, target Target) error {
	lay, err := scan(srcPath, target)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", srcPath, err)
	}

	switch {
	case len(lay.hoisted) > 0:
		return rewrite(srcPath, dstPath, target, lay, compressionLevel)
	case lay.hasRootBin || len(lay.platforms) == 0:
		// Already flat: dst would be byte-identical to src, so link it rather
		// than rewriting gigabytes to no effect. Both names are treated as
		// immutable from here on -- pack only ever reads them.
		return fsutil.LinkOrCopy(srcPath, dstPath)
	default:
		// Refusing here matters: copying through would produce a .cnb that
		// only fails much later, inside the detect phase of a real build.
		return fmt.Errorf("archive ships no %s binaries, only %s",
			target, strings.Join(sortedKeys(lay.platforms), ", "))
	}
}

// layout is what one header-only pass over an archive reveals about its shape.
type layout struct {
	// hasRootBin marks a single-arch archive that needs no rewriting.
	hasRootBin bool
	// platforms holds every "<os>/<arch>" that contains a bin directory.
	platforms map[string]bool
	// hoisted maps each of the target's files to the name it takes at the root.
	hoisted map[string]bool
}

func scan(srcPath string, target Target) (layout, error) {
	lay := layout{platforms: map[string]bool{}, hoisted: map[string]bool{}}
	prefix := target.prefix()

	err := walk(srcPath, func(hdr *tar.Header) error {
		name := normalize(hdr.Name)
		if name == "" {
			return nil
		}

		if rest := strings.TrimPrefix(name, prefix); rest != name && rest != "" {
			lay.hoisted[rest] = true
		}

		parts := strings.Split(name, "/")
		if parts[0] == "bin" && len(parts) > 1 {
			lay.hasRootBin = true
		}
		if len(parts) > 2 && parts[2] == "bin" {
			lay.platforms[parts[0]+"/"+parts[1]] = true
		}
		return nil
	})
	return lay, err
}

func rewrite(srcPath, dstPath string, target Target, lay layout, level int) error {
	prefix := target.prefix()

	// Only the directories that exist to hold per-platform trees are dropped,
	// so an archive that happens to have an unrelated top-level directory
	// keeps it.
	platformRoots := map[string]bool{}
	for p := range lay.platforms {
		platformRoots[strings.SplitN(p, "/", 2)[0]] = true
	}

	rename := func(name string) (string, bool) {
		name = normalize(name)
		if name == "" {
			return "", false
		}
		if rest := strings.TrimPrefix(name, prefix); rest != name {
			// rest == "" is the platform directory entry itself, which becomes
			// the archive root and so has nothing to be written as.
			return rest, rest != ""
		}
		if platformRoots[strings.SplitN(name, "/", 2)[0]] {
			return "", false
		}
		// A root entry of the same name is the less specific one; the hoisted
		// copy replaces it, mirroring pack's CopyConfigFile.
		return name, !lay.hoisted[name]
	}

	return transform(srcPath, dstPath, level, rename)
}

// transform streams src to dst, renaming or dropping entries as rename says.
// It never holds an entry in memory: an offline component archive carries every
// vendored dependency the buildpack declares and runs to several gigabytes.
func transform(srcPath, dstPath string, level int, rename func(string) (string, bool)) (err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	gr, err := gzip.NewReader(bufio.NewReaderSize(src, bufSize))
	if err != nil {
		return err
	}
	defer gr.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dstPath)
		}
	}()

	bw := bufio.NewWriterSize(dst, bufSize)
	gw, err := gzip.NewWriterLevel(bw, level)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gw)

	// One buffer for every entry: io.Copy would otherwise allocate a fresh 32KiB
	// for each, since neither tar.Reader nor tar.Writer offers a fast path.
	copyBuf := make([]byte, bufSize)

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		name, keep := rename(hdr.Name)
		if !keep {
			continue
		}

		out := *hdr
		out.Name = name
		if out.Typeflag == tar.TypeDir {
			out.Name += "/"
		}
		// A copied "path" record would silently override the new Name, and
		// nothing jam produces needs the extended attributes these carry.
		out.PAXRecords = nil
		out.Format = tar.FormatUnknown

		// Symlink targets are resolved against the link's own directory, and
		// hoisting moves a directory whole, so bin/detect -> run survives
		// untouched. Hard link targets are archive-relative and do move.
		if out.Typeflag == tar.TypeLink {
			if target, ok := rename(out.Linkname); ok {
				out.Linkname = target
			}
		}

		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if hdr.Size > 0 {
			if _, err := io.CopyBuffer(tw, tr, copyBuf); err != nil {
				return err
			}
		}
	}

	// Closed innermost first: tar flushes into gzip, gzip into the buffer, and
	// only then does the buffer reach the file.
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return bw.Flush()
}

func walk(srcPath string, fn func(*tar.Header) error) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(bufio.NewReaderSize(f, bufSize))
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(hdr); err != nil {
			return err
		}
	}
}

// normalize reduces a tar entry name to a slash-separated relative path with no
// trailing slash, so that "./bin/" and "bin" compare equal.
func normalize(name string) string {
	name = path.Clean(name)
	if name == "." || name == "/" {
		return ""
	}
	return strings.TrimPrefix(name, "/")
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
