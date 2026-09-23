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

url="$("$binary" --home "$scratch/vault" daemon status | sed -n 's/^msgvault running at //p')"
[[ "$url" == http://127.0.0.1:* ]]
curl --fail --silent --show-error "$url/" -o "$scratch/index.html"
grep -qi '<!doctype html>' "$scratch/index.html"
