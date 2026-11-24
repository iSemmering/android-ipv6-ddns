#!/bin/sh
set -eu

cd "$(dirname "$0")"
go test ./...
go vet ./...

stage=$(mktemp -d "${TMPDIR:-/tmp}/ipv6-ddns-stage.XXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
cp -R module/. "$stage"
cp LICENSE "$stage/LICENSE"
mkdir -p "$stage/bin"
for arch in arm64 arm amd64; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOARM=7 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$stage/bin/ddns-$arch" ./cmd/ddns
done
chmod 0755 "$stage/bin/"* "$stage/"*.sh
for script in "$stage/"*.sh; do sh -n "$script"; done

version=$(sed -n 's/^version=//p' module/module.prop)
output_dir="$(pwd)/dist"
mkdir -p "$output_dir"
target="$output_dir/Android-IPv6-DDNS-v${version}.zip"
temporary="$(mktemp "${TMPDIR:-/tmp}/ipv6-ddns-zip.XXXXXX")"
trap 'rm -rf "$stage"; rm -f "$temporary" "$temporary.zip"' EXIT HUP INT TERM
(cd "$stage" && zip -q -X -r "$temporary.zip" .)
unzip -tq "$temporary.zip"
mv "$temporary.zip" "$target"
printf '%s\n' "$target"
