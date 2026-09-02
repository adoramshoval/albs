#!/bin/sh
#
# Prints the path of a Go toolchain capable of building this module.
#
# The go directive in go.mod may name a release newer than the `go` on PATH.
# Go 1.21+ handles that itself by downloading the required toolchain, so what
# actually matters is the *effective* version a candidate reports from inside
# this module -- not the version it was installed as. Toolchains older than
# 1.21 have no such mechanism and fail with confusing "package slices is not in
# GOROOT" errors instead, which is exactly what this guard exists to prevent.
#
# POSIX sh only: no arrays, no bashisms, no sort -V. Runs anywhere Go does.

set -eu

cd "$(dirname "$0")/.." || exit 1

required="$(awk '/^go[[:space:]]+[0-9]/ { print $2; exit }' go.mod)"
if [ -z "$required" ]; then
  echo "preflight: no go directive found in go.mod" >&2
  exit 1
fi

# version_ge HAVE WANT -- true when HAVE >= WANT, comparing dotted fields
# numerically. awk rather than `sort -V`, which is absent from some shells'
# coreutils and behaves differently between GNU and BSD.
version_ge() {
  awk -v have="$1" -v want="$2" '
    BEGIN {
      nh = split(have, h, ".")
      nw = split(want, w, ".")
      for (i = 1; i <= nw; i++) {
        hv = (i <= nh) ? h[i] + 0 : 0
        wv = w[i] + 0
        if (hv > wv) exit 0
        if (hv < wv) exit 1
      }
      exit 0
    }'
}

# Candidates, most specific first: an explicit GO, then every go on PATH, then
# the toolchains that common installers lay down on Linux and macOS. Listing a
# path costs nothing on systems where it does not exist.
report=""
seen=""

# try CANDIDATE -- if it can build the module, print it and exit; otherwise
# record it and return. Checking inline avoids building a list that would have
# to be word-split, which would mishandle paths containing spaces.
try() {
  [ -n "$1" ] || return 0
  [ -x "$1" ] || return 0

  case "$seen" in
    *"|$1|"*) return 0 ;;
  esac
  seen="$seen|$1|"

  # Queried from the module root so automatic toolchain selection applies.
  effective="$("$1" env GOVERSION 2>/dev/null | sed 's/^go//')" || effective=""
  [ -n "$effective" ] || return 0

  if version_ge "$effective" "$required"; then
    echo "$1"
    exit 0
  fi
  report="$report  $1 -> go$effective
"
}

# An explicitly requested toolchain is authoritative: if it cannot build the
# module, say so rather than quietly substituting a different one.
if [ -n "${GO:-}" ]; then
  if [ ! -x "$GO" ]; then
    echo "preflight: GO=$GO is not an executable" >&2
    exit 1
  fi
  effective="$("$GO" env GOVERSION 2>/dev/null | sed 's/^go//')" || effective=""
  if [ -z "$effective" ]; then
    echo "preflight: GO=$GO did not report a version" >&2
    exit 1
  fi
  if version_ge "$effective" "$required"; then
    echo "$GO"
    exit 0
  fi
  echo "preflight: GO=$GO is go$effective, but go.mod requires go $required" >&2
  exit 1
fi

try "${GOROOT:+$GOROOT/bin/go}"

# Every PATH entry, not just the first match: an unusable go earlier on PATH
# must not hide a usable one behind it.
old_ifs="$IFS"
IFS=:
for dir in $PATH; do
  [ -n "$dir" ] || dir="."
  try "$dir/go"
done
IFS="$old_ifs"

# Distribution and installer defaults.
try /usr/local/go/bin/go        # upstream tarball (Linux, macOS)
try /usr/lib/go/bin/go          # Arch, Alpine
try /usr/lib/golang/bin/go      # Fedora, RHEL
try /usr/local/lib/go/bin/go
try /opt/go/bin/go
try /snap/bin/go                # Ubuntu snap
try /opt/homebrew/bin/go        # Homebrew, Apple silicon
try /opt/homebrew/opt/go/bin/go
try /usr/local/opt/go/bin/go    # Homebrew, Intel macOS

# Toolchains installed via `go install golang.org/dl/goX.Y@latest`.
for sdk in "${HOME:-}"/sdk/*/bin/go; do
  if [ -x "$sdk" ]; then
    try "$sdk"
  fi
done

{
  echo "preflight: no Go toolchain found that can build this module."
  echo "  go.mod requires: go $required"
  if [ -n "$report" ]; then
    echo "  candidates checked:"
    printf '%s' "$report"
  else
    echo "  no go binary found on PATH or in the usual install locations."
  fi
  echo
  echo "  Install a newer Go, or point make at one explicitly:"
  echo "      make build GO=/path/to/go"
  echo "  Any Go >= 1.21 will do: it downloads the required toolchain on demand."
} >&2
exit 1
