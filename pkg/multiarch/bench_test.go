package multiarch

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// Shape of the fixture. A real offline component archive is dominated by
// vendored dependency tarballs -- cpython v1.18.41 carries 51 of them at ~53MB
// each -- next to a 53KB buildpack.toml and a handful of tiny binaries. Sizes
// here are scaled down to keep the benchmark runnable; what is being measured
// is the ratio between compression levels, which depends on the character of
// the payload rather than its size.
const (
	benchBlobs         = 20
	benchBlobSize      = 1 << 20
	benchBuildpackTOML = 53 << 10
	// Real component binaries are a few tens of MB against gigabytes of
	// dependencies, so they are a rounding error in the payload. Keeping that
	// proportion matters: filling them with zeros, or sizing them as a large
	// share of the archive, would hand the higher compression levels a win the
	// real workload never offers them.
	benchBinarySize = 512 << 10
)

// writeBenchFixture builds an archive shaped like jam's offline output.
//
// The blobs are pseudo-random, standing in for already-gzipped dependency
// tarballs: both are incompressible, which is the property that decides whether
// spending CPU on deflate buys anything. buildpack.toml is repetitive TOML, so
// the fixture also carries content that genuinely does compress.
func writeBenchFixture(tb testing.TB, path string) {
	tb.Helper()

	f, err := os.Create(path)
	if err != nil {
		tb.Fatalf("create: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	toml := make([]byte, 0, benchBuildpackTOML)
	for len(toml) < benchBuildpackTOML {
		toml = append(toml, "  [[metadata.dependencies]]\n    id = \"python\"\n    stacks = [\"*\"]\n"...)
	}
	writeBenchEntry(tb, tw, "buildpack.toml", toml)

	rng := rand.New(rand.NewSource(1))
	blob := make([]byte, benchBlobSize)
	for i := range benchBlobs {
		rng.Read(blob)
		writeBenchEntry(tb, tw, fmt.Sprintf("dependencies/dep-%02d.tgz", i), blob)
	}

	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if err := tw.WriteHeader(&tar.Header{
			Name: platform + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			tb.Fatalf("header: %v", err)
		}
		rng.Read(blob[:benchBinarySize])
		writeBenchEntry(tb, tw, platform+"/bin/run", blob[:benchBinarySize])
		if err := tw.WriteHeader(&tar.Header{
			Name: platform + "/bin/detect", Typeflag: tar.TypeSymlink, Linkname: "run", Mode: 0o755,
		}); err != nil {
			tb.Fatalf("header: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		tb.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		tb.Fatalf("close gzip: %v", err)
	}
}

func writeBenchEntry(tb testing.TB, tw *tar.Writer, name string, body []byte) {
	tb.Helper()

	if err := tw.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		tb.Fatalf("header %s: %v", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		tb.Fatalf("body %s: %v", name, err)
	}
}

// BenchmarkCompressionLevel answers whether re-deflating an offline archive at
// the default level earns its CPU, given the payload is already compressed.
//
// It reports out_MB alongside the timing, because the decision is a trade
// between the two: report both as ratios against Default, since absolute
// figures do not carry across machines.
func BenchmarkCompressionLevel(b *testing.B) {
	levels := []struct {
		name  string
		level int
	}{
		{"Default", gzip.DefaultCompression},
		{"BestSpeed", gzip.BestSpeed},
		{"NoCompression", gzip.NoCompression},
	}

	src := filepath.Join(b.TempDir(), "src.tgz")
	writeBenchFixture(b, src)

	target := Target{OS: "linux", Arch: "amd64"}
	lay, err := scan(src, target)
	if err != nil {
		b.Fatalf("scan: %v", err)
	}

	for _, lvl := range levels {
		b.Run(lvl.name, func(b *testing.B) {
			dst := filepath.Join(b.TempDir(), "dst.tgz")

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := rewrite(src, dst, target, lay, lvl.level); err != nil {
					b.Fatalf("rewrite: %v", err)
				}
			}
			b.StopTimer()

			info, err := os.Stat(dst)
			if err != nil {
				b.Fatalf("stat: %v", err)
			}
			b.ReportMetric(float64(info.Size())/(1<<20), "out_MB")
		})
	}
}

// BenchmarkScan measures the header-only pass on its own, since the two-pass
// design pays for it on every rewrite.
func BenchmarkScan(b *testing.B) {
	src := filepath.Join(b.TempDir(), "src.tgz")
	writeBenchFixture(b, src)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := scan(src, Target{OS: "linux", Arch: "amd64"}); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}
