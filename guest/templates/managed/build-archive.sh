#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: build-archive.sh PREPARED_DEBIAN_ROOTFS OUTPUT.tar" >&2
    exit 2
fi
src=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$src/../../.." && pwd)
base=$(realpath "$1")
output=$(realpath -m "$2")
case "$output" in
    *.tar) ;;
    *) echo "output must end in .tar" >&2; exit 2 ;;
esac
case "$output" in
    "$base"/*) echo "output must be outside the base rootfs" >&2; exit 2 ;;
esac
if [ "$(id -u)" -ne 0 ]; then
    echo "root is required to preserve Debian file ownership in the archive" >&2
    exit 2
fi
if [ ! -x "$base/usr/bin/runsvdir" ] || [ ! -x "$base/usr/sbin/sshd" ] || [ ! -x "$base/usr/bin/flock" ]; then
    echo "prepare the Debian rootfs with runit, openssh-server, and util-linux" >&2
    exit 2
fi

build=$(mktemp -d)
archive=$(mktemp "$(dirname -- "$output")/.sandbox-image.XXXXXX")
trap 'rm -rf "$build"; rm -f "$archive"' EXIT
mkdir "$build/rootfs" "$build/bin"
cp -a "$base/." "$build/rootfs"
cd "$repo"
for command in taskd taskctl task-runner; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "${GO_BINARY:-go}" build -trimpath -buildvcs=false -o "$build/bin/$command" "./cmd/sandbox-$command"
done
"$src/install.sh" "$build/rootfs" "$build/bin"

tar --format=gnu --numeric-owner --sort=name -C "$build/rootfs" -cf "$build/layer.tar" .
hash=$(sha256sum "$build/layer.tar" | cut -d ' ' -f 1)
cat >"$build/config.json" <<EOF
{"architecture":"amd64","os":"linux","config":{"Cmd":["/bin/sleep","infinity"],"WorkingDir":"/workspace","User":"0:0"},"rootfs":{"type":"layers","diff_ids":["sha256:$hash"]}}
EOF
cat >"$build/manifest.json" <<'EOF'
[{"Config":"config.json","RepoTags":["sandbox/debian-managed:v1"],"Layers":["layer.tar"]}]
EOF
tar --format=gnu --numeric-owner -C "$build" -cf "$archive" manifest.json config.json layer.tar
chmod 0644 "$archive"
mv -T "$archive" "$output"
sha256sum "$output"
