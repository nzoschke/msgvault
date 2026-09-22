#!/usr/bin/env bash
set -euo pipefail

binary="$(realpath "${1:?binary path required}")"
scratch="$(mktemp -d)"
cleanup() {
  "$binary" --home "$scratch/vault" daemon stop >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
mkdir -p "$scratch/home"
export HOME="$scratch/home"
"$binary" version
"$binary" sync-external --help >/dev/null
"$binary" --home "$scratch/vault" init-db
"$binary" --home "$scratch/vault" build-cache --full-rebuild
test "$("$binary" --home "$scratch/vault" query --format csv 'SELECT COUNT(*) AS messages FROM messages')" = "$(printf 'messages\n0')"
