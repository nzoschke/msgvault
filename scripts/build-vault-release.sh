#!/usr/bin/env bash
set -euo pipefail

version="${1:?release version required}"
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-hc\.[0-9]+$ ]] || { echo "invalid release version" >&2; exit 1; }
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${2:-$repo_root/dist}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
cd "$repo_root"
architecture="$(go env GOARCH)"
[[ "$(go env GOOS)" == linux && "$architecture" =~ ^(amd64|arm64)$ ]] || { echo "Linux amd64 or arm64 required" >&2; exit 1; }
commit="$(git rev-parse HEAD)"
build_date="$(git show -s --format=%cI HEAD)"
build_epoch="$(git show -s --format=%ct HEAD)"
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT
CGO_ENABLED=1 GOWORK=off go build -tags 'fts5 sqlite_vec' -trimpath -buildvcs=false \
  -ldflags "-s -w -X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=$version -X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=$commit -X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=$build_date" \
  -o "$build_dir/msgvault" ./cmd/msgvault
"$build_dir/msgvault" sync-external --help >/dev/null
asset="msgvault_${version}_linux_${architecture}.tar.gz"
tar --sort=name --mtime="@$build_epoch" --owner=0 --group=0 --numeric-owner -C "$build_dir" -czf "$output/$asset" msgvault
(cd "$output" && sha256sum "$asset" > "$asset.sha256")
