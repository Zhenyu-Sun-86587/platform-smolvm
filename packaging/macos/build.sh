#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
    echo "Build Sandbox Menu.app on macOS with Xcode Command Line Tools." >&2
    exit 1
fi

source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository=$(cd "$source_dir/../.." && pwd)
output=${1:-"$PWD/.build/macos"}
mkdir -p "$output"
output=$(cd "$output" && pwd)
final="$output/Sandbox Menu.app"
if [[ -e "$final" || -L "$final" ]]; then
    echo "Choose an output directory without an existing Sandbox Menu.app." >&2
    exit 1
fi

build_dir=$(mktemp -d "$output/.sandbox-menu.XXXXXXXX")
trap 'rm -rf "$build_dir"' EXIT
app="$build_dir/Sandbox Menu.app"
contents="$app/Contents"
mkdir -p "$contents/MacOS" "$contents/Helpers"
cp "$source_dir/Info.plist" "$contents/Info.plist"

arch=$(uname -m)
case "$arch" in
    arm64) go_arch=arm64 ;;
    x86_64) go_arch=amd64 ;;
    *) echo "Unsupported Mac architecture $arch" >&2; exit 1 ;;
esac

(
    cd "$repository"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$go_arch" go build -mod=readonly -buildvcs=false -trimpath -o "$contents/Helpers/sandboxctl" ./cmd/sandboxctl
)
xcrun swiftc -swift-version 5 -O -target "$arch-apple-macos13.0" \
    "$source_dir/CLI.swift" "$source_dir/main.swift" -o "$contents/MacOS/SandboxMenu"
/usr/bin/plutil -lint "$contents/Info.plist"

identity=${SANDBOX_MENU_SIGN_IDENTITY:--}
declare -a sign_options=()
if [[ "$identity" != - ]]; then
    sign_options=(--options runtime --timestamp)
else
    sign_options=(--timestamp=none)
fi
/usr/bin/codesign --force --sign "$identity" "${sign_options[@]}" "$contents/Helpers/sandboxctl"
/usr/bin/codesign --force --sign "$identity" "${sign_options[@]}" "$app"
/usr/bin/codesign --verify --deep --strict --verbose=2 "$app"
mv "$app" "$final"
printf '%s\n' "$final"
