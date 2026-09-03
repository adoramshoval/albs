package preflight

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// buildpackTOMLName is the file every buildpack carries at its root, in a
// source tree and in a packaged archive alike.
const buildpackTOMLName = "buildpack.toml"

// readLimit caps how much of a buildpack.toml is read out of an archive. The
// real files run to tens of kilobytes; the limit exists so a malformed entry
// in a multi-gigabyte offline archive cannot be read into memory whole.
const readLimit = 8 << 20

// bufSize matches the reader size multiarch uses for the same archives.
const bufSize = 1 << 20

// BuildpackTOML is the subset of a buildpack.toml that coverage depends on.
// Everything else in the file is ignored, so an unrecognised key is not an
// error -- these files carry a good deal that albs has no interest in.
type BuildpackTOML struct {
	API       string    `toml:"api"`
	Buildpack Buildpack `toml:"buildpack"`
	Order     []Group   `toml:"order"`
	Metadata  Metadata  `toml:"metadata"`
}

type Buildpack struct {
	ID      string `toml:"id"`
	Version string `toml:"version"`
}

// Group is one [[order]] entry: a set of buildpacks the lifecycle tries
// together. Detection takes the first group whose non-optional members all
// pass, so groups are alternatives rather than a single required set.
type Group struct {
	Group []GroupEntry `toml:"group"`
}

type GroupEntry struct {
	ID       string `toml:"id"`
	Version  string `toml:"version"`
	Optional bool   `toml:"optional"`
}

type Metadata struct {
	Dependencies []Dependency `toml:"dependencies"`
}

// Dependency is one [[metadata.dependencies]] entry. Stacks and Arch are the
// two fields coverage turns on, and jam ignores both -- it downloads every
// entry regardless, which is why a bundle is stack-agnostic and why this
// check has to happen outside jam.
//
// URI and Source together say whether the artifact is compiled or not. Paketo
// records both for every dependency, and when they name the same file the
// "artifact" is the source tarball.
type Dependency struct {
	ID      string   `toml:"id"`
	Version string   `toml:"version"`
	Arch    string   `toml:"arch"`
	Stacks  []string `toml:"stacks"`
	URI     string   `toml:"uri"`
	Source  string   `toml:"source"`
}

// ParseDir reads the buildpack.toml at the root of a checked-out buildpack.
func ParseDir(dir string) (BuildpackTOML, error) {
	bp, _, err := ReadDir(dir)
	return bp, err
}

// ReadDir reads a checked-out buildpack.toml, returning both the decoded form
// and the bytes it came from.
//
// The caller keeps the bytes because packaging destroys them: jam rewrites
// every dependency uri to a local path and drops source, so this is the only
// moment at which a component's provenance can be preserved.
func ReadDir(dir string) (BuildpackTOML, []byte, error) {
	p := filepath.Join(dir, buildpackTOMLName)
	data, err := os.ReadFile(p)
	if err != nil {
		return BuildpackTOML{}, nil, fmt.Errorf("reading %s: %w", p, err)
	}
	bp, err := decode(data, p)
	if err != nil {
		return BuildpackTOML{}, nil, err
	}
	return bp, data, nil
}

// Parse decodes a buildpack.toml held in memory, such as one recovered from
// the cache.
func Parse(data []byte, source string) (BuildpackTOML, error) {
	return decode(data, source)
}

// ParseArchive reads the buildpack.toml from the root of a packaged buildpack.
//
// This is the path taken on a cache hit, where jam's output is reused and the
// component is never cloned. It also reports what was actually vendored rather
// than what the source declares, which is the stronger claim of the two.
func ParseArchive(tgzPath string) (BuildpackTOML, error) {
	f, err := os.Open(tgzPath)
	if err != nil {
		return BuildpackTOML{}, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(bufio.NewReaderSize(f, bufSize))
	if err != nil {
		return BuildpackTOML{}, fmt.Errorf("reading %s: %w", tgzPath, err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return BuildpackTOML{}, fmt.Errorf("reading %s: %w", tgzPath, err)
		}
		if normalize(hdr.Name) != buildpackTOMLName {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, readLimit))
		if err != nil {
			return BuildpackTOML{}, fmt.Errorf("reading %s from %s: %w", buildpackTOMLName, tgzPath, err)
		}
		return decode(data, tgzPath+"!"+buildpackTOMLName)
	}

	return BuildpackTOML{}, fmt.Errorf("no %s at the root of %s", buildpackTOMLName, tgzPath)
}

func decode(data []byte, source string) (BuildpackTOML, error) {
	var bp BuildpackTOML
	if err := toml.Unmarshal(data, &bp); err != nil {
		return BuildpackTOML{}, fmt.Errorf("decoding %s: %w", source, err)
	}
	return bp, nil
}

// normalize reduces a tar entry name to a slash-separated relative path, so
// that "./buildpack.toml" and "buildpack.toml" compare equal.
func normalize(name string) string {
	name = path.Clean(name)
	if name == "." || name == "/" {
		return ""
	}
	return strings.TrimPrefix(name, "/")
}
