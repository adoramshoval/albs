<p align="center">
  <img src="docs/logo.svg" width="440" alt="albs">
</p>

# albs

`albs` packages [Paketo](https://paketo.io) [meta-buildpacks](https://buildpacks.io/docs/for-buildpack-authors/concepts/composite-buildpack/) into self-contained offline [`.cnb`](https://github.com/buildpacks/spec/blob/main/distribution.md) bundles — [Cloud Native Buildpacks](https://buildpacks.io) packages that carry every dependency they need.

## Motivation

Cloud Native Buildpacks fetch what they need while a build runs, downloading language runtimes, package managers and libraries from the public internet. Where there is no egress, or where egress reaches only an internal mirror, those builds cannot complete. A buildpack that already carries its dependencies can.

Paketo publishes [`jam`](https://github.com/paketo-buildpacks/jam) for this purpose: `jam pack --offline` downloads everything a buildpack declares and rewrites its configuration to point at the vendored copies. It operates on one buildpack at a time, and on the dependencies that buildpack declares for itself. A meta-buildpack declares none — it exists to reference other buildpacks — so packaging one offline completes successfully while vendoring nothing, and the resulting composite still reaches for the network at build time. Producing a genuinely offline composite therefore means resolving every component back to its source repository, checking out the revision the meta-buildpack pins, vendoring that component on its own, and assembling the results into a single package. Paketo's Python meta-buildpack pins more than a dozen components; done by hand, that is substantial work, and it recurs at every upgrade.

`albs` performs that work. It reads a meta-buildpack's [`package.toml`](https://buildpacks.io/docs/reference/config/package-config/), resolves each component buildpack back to its source Git repository, vendors each one offline with `jam`, and assembles the result into a single composite `.cnb` package using the [`pack`](https://github.com/buildpacks/pack) client library.

The resulting bundle builds with no network access at all, at the cost of size: every component contributes every dependency version it declares, rather than only the versions a particular application resolves to, and multi-architecture buildpacks carry a copy per target. It is large by construction — a smaller bundle would be one that still expected the network.

## Features

- **Source resolution**: maps component references back to Git repositories via the [`org.opencontainers.image.source`](https://github.com/opencontainers/image-spec/blob/main/annotations.md) OCI label, a user-supplied mapping file, or the standard Paketo naming convention.
- **Two dependency formats**: handles both `docker://` image URIs and [CNB registry](https://registry.buildpacks.io) URNs (`urn:cnb:registry:<namespace>/<name>@<version>`). Paketo's tagged releases use the latter; its `main` branch uses the former.
- **Ref discovery**: components are pinned by image tag (`cpython:1.8.7`) while the source repository tags the release `v1.8.7`. `albs` lists the remote's refs and matches them rather than assuming a convention.
- **In-process Git**: cloning uses [`go-git`](https://github.com/go-git/go-git); no `git` binary is required.
- **Concurrent builds**: components are vendored in parallel, bounded by `--concurrency`.
- **Caching**: built archives are cached, keyed by source ref and salted with the `jam` build that produced them. Writes are atomic.
- **Registry credentials**: an existing [`docker login`](https://docs.docker.com/reference/cli/docker/login/) is picked up automatically for private and mirrored registries.

## Prerequisites

- **[Go](https://go.dev/dl/)** — any release ≥ 1.21. `go.mod` declares `go 1.27`; Go 1.21 and later download that toolchain automatically. See [Toolchain](#toolchain).
- **[jam](https://github.com/paketo-buildpacks/jam)** — Paketo's packaging tool. Its packaging logic lives in an unimportable `internal/` package upstream, so `albs` invokes it as a subprocess and checks for it before doing any work.

  ```bash
  go install github.com/paketo-buildpacks/jam/v2@latest
  ```

  Ensure `$(go env GOPATH)/bin` is on your `PATH`.
- **[Docker](https://docs.docker.com/engine/install/)** — a running daemon. The `pack` library constructs a Docker client from the environment unconditionally, so this is required even for file-format packages.

`git` is *not* required.

## Building

```bash
make build
```

The binary is written to `bin/albs`. Run `make help` for the full target list.

### Toolchain

`make` does not assume the `go` on your `PATH` is usable. `scripts/preflight-go.sh` inspects each candidate toolchain's *effective* version from inside the module — which accounts for [Go's automatic toolchain switching](https://go.dev/doc/toolchain) — and selects the first that satisfies the `go` directive in `go.mod`.

If none qualifies, the build stops with the versions it found rather than a wall of `package slices is not in GOROOT` errors from inside your dependencies. To use a specific toolchain:

```bash
make build GO=/path/to/go
```

## Usage

```bash
./bin/albs \
  --git-url https://github.com/paketo-buildpacks/python \
  --tag v2.9.2 \
  --output ./python-offline.cnb
```

Run `albs --help` for the full flag list.

| Flag | Short | Default | Description |
| --- | --- | --- | --- |
| `--git-url` | `-u` | *(required)* | Git repository URL of the meta-buildpack. |
| `--tag` | `-t` | *(required)* | Released Git tag to build. Also supplies the version stamped into the package. |
| `--output` | `-o` | `./meta-buildpack-offline.cnb` | Path for the generated `.cnb` archive. |
| `--cache-dir` | | `./.cache` | Directory for cached component archives. |
| `--repo-map` | | | Path to a JSON or YAML file mapping component references to Git URLs. |
| `--concurrency` | `-j` | *(logical CPUs)* | Maximum parallel component builds. |
| `--verbose` | `-v` | `false` | Verbose logging, for `albs` and `pack` alike. |

### Repository mapping

Supply `--repo-map` when a component image lacks the `org.opencontainers.image.source` label, or when pulling from a private or mirrored registry. JSON and YAML are both accepted, distinguished by file extension.

```yaml
# Keys may be a bare repository, the reference as written in package.toml,
# or a reference with an explicit tag; all three forms are matched.
gcr.io/paketo-buildpacks/cpython: "https://github.com/paketo-buildpacks/cpython"
paketo-buildpacks/pip: "https://github.com/paketo-buildpacks/pip"
my-registry.internal/buildpacks/custom-node: "https://git.internal/buildpacks/custom-node"
```

## How it works

1. **Clone the meta-buildpack.** `--tag` is matched against the remote's refs and cloned into a temporary workspace.
2. **Parse `package.toml`** for the component references.
3. **Resolve each component's source repository**, in order: the `--repo-map`; the `org.opencontainers.image.source` label on the image manifest, fetched with ambient registry credentials (skipped for CNB registry URNs, which name no image); the Paketo naming convention.
4. **Vendor each component.** The pinned version is matched against the source repository's refs, since Paketo's Git tags carry a `v` prefix its image tags do not. Cached archives are reused; the rest are cloned and packaged with `jam pack --offline --version <tag>`. The version is required — component [`buildpack.toml`](https://buildpacks.io/docs/reference/config/buildpack-config/) files carry none of their own, and the meta-buildpack's order groups pin exact versions.
5. **Package the meta-buildpack itself.** Its checked-in `[buildpack] uri` points at `build/buildpack.tgz`, an artifact of the upstream repository's packaging script that a fresh clone does not contain. Pointing `pack` at the source directory instead is not sufficient, because `buildpack.toml` carries no `version` and `pack` requires one. So the meta-buildpack is run through `jam` exactly as its components are, stamped with the version from `--tag`.
6. **Rewrite the configuration in memory.** Dependency URIs point at the vendored archives and `[buildpack] uri` at the packaged meta-buildpack. A missing `[platform] os` defaults to `linux`, matching `pack`'s own config reader.

   `package.toml` is deliberately *not* rewritten on disk. `pack` resolves relative URIs against `RelativeBaseDir`, so the file is never read again, and marshalling the configuration back to TOML would emit empty `image`, `extension` and `platform` keys the original never had.
7. **Assemble the composite** `.cnb` and remove the workspace.

## Known limitations

- Only Paketo buildpacks are supported in practice. `package.toml` is read through `pack`'s own spec types and is portable; vendoring is not. `jam` reads Paketo's `[metadata.dependencies]` schema, a table the spec leaves free-form. `albs` further assumes a component declares no version of its own — it stamps the Git tag — and that the meta-buildpack's `[buildpack] uri` names a build artifact absent from a fresh clone. Supporting another ecosystem means a different packer, not a different `package.toml` parser.
- A running Docker daemon is required. `pack`'s client constructs one from the environment unconditionally; avoiding it would mean bypassing `pkg/client` entirely.
- `jam` is invoked as a subprocess, because its packaging logic is in an `internal/` package upstream.
- `--tag` must name a released tag. Paketo `buildpack.toml` files carry no version, so there is nothing else to stamp the package with.
- `jam` binaries built with `go install` report no version. `albs` falls back to fingerprinting the binary for cache keying and skips the minimum-version check, warning as it does so.
