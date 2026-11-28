#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 && $# != 4 ]]; then
    echo "Usage: $0 VERSION OFFICIAL_RUNTIME_ARCHIVE OUTPUT_DIRECTORY [GIT_REF]" >&2
    exit 2
fi
version=$1
runtime_archive=$(realpath -e "$2")
output=$(realpath -m "$3")
ref=${4:-HEAD}
[[ $version =~ ^[0-9]+[.][0-9]+[.][0-9]+([.-][a-zA-Z0-9]+)*$ ]]
[[ -f "$runtime_archive" ]]
repo=$(cd -- "$(dirname -- "$0")/../.." && pwd)
commit=$(git -C "$repo" rev-parse --verify --end-of-options "${ref}^{commit}")
source_date=$(git -C "$repo" show -s --format=%cI "$commit")
bundle=sandbox-platform-$version-linux-amd64
archive=$bundle.tar.gz
mkdir -p -- "$output"
[[ ! -e $output/$archive && ! -e $output/$archive.sha256 ]] || {
    echo "Release archive or checksum already exists" >&2
    exit 1
}
work=$(mktemp -d "$output/.sandbox-archive.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/source" "$work/package"
git -C "$repo" archive "$commit" | tar -x -C "$work/source"
manifest=$work/source/packaging/runtime-manifest.json
asset=$(jq -er '.asset' "$manifest")
expected=$(jq -er '.sha256' "$manifest")
actual=$(sha256sum -- "$runtime_archive" | cut -d ' ' -f 1)
[[ $asset =~ ^[a-z0-9._-]+[.]tar[.]gz$ && $actual == "$expected" ]] || {
    echo "Official runtime archive checksum mismatch" >&2
    exit 1
}
release=$work/package/$bundle
"$work/source/packaging/install/make-release.sh" "$version" "$release"
mkdir "$release/runtime"
install -m 0644 "$runtime_archive" "$release/runtime/$asset"
sed -e 's|\[guest/templates/managed\](../../guest/templates/managed)|guest/templates/managed in the source repository|' \
    -e 's|../managed-template-gates.json|managed-template-gates.json|' \
    -e 's|../../docs/phase1/LOCAL_GUEST_EVIDENCE.md|LOCAL_GUEST_EVIDENCE.md|' \
    "$work/source/packaging/install/README.md" >"$release/MAINTENANCE.md"
install -m 0644 "$work/source/packaging/managed-template-gates.json" "$release/managed-template-gates.json"
evidence="$work/source/packaging/install/LOCAL_GUEST_EVIDENCE.md"
if [[ ! -f $evidence ]]; then
    evidence="$work/source/docs/phase1/LOCAL_GUEST_EVIDENCE.md"
fi
install -m 0644 "$evidence" "$release/LOCAL_GUEST_EVIDENCE.md"
jq -n --arg version "$version" --arg commit "$commit" --arg source_date "$source_date" \
    --arg go "$(go env GOVERSION)" --arg asset "runtime/$asset" --arg digest "$actual" \
    '{version:$version,source_commit:$commit,source_date:$source_date,go_version:$go,runtime_archive:$asset,runtime_sha256:$digest}' \
    >"$release/BUILD-INFO.json"
cat >"$release/INSTALL.md" <<INSTALL
# Sandbox Platform $version

This archive includes the official smolvm runtime and the verified default managed template. The build source is commit \`$commit\`.

Save an owner password in a local file with mode 0600. Run these commands from the extracted release directory.

\`\`\`sh
./install.sh --plan --manifest ./release-manifest.json \\
  --config ./config.example.yaml --runtime-archive ./runtime/$asset
sudo ./install.sh --manifest ./release-manifest.json \\
  --config ./config.example.yaml --runtime-archive ./runtime/$asset \\
  --owner-password-file /secure/path/owner-password
sudo /usr/local/bin/sandboxctl doctor --json
\`\`\`

The default console is http://127.0.0.1:8443. Sign in as \`owner\` using the password provided at installation. The installed CLI is \`/usr/local/bin/sandboxctl\`. Use \`SANDBOX_URL=http://127.0.0.1:8443\` with the default console when running authenticated API commands from the CLI. See MAINTENANCE.md for backups and upgrades and LOCAL_GUEST_EVIDENCE.md for the managed template checks.
INSTALL
(
    cd "$work"
    tar --format=gnu --sort=name --numeric-owner --owner=0 --group=0 --mtime="$source_date" \
        -C "$work/package" -cf - "$bundle" | gzip -n >"$archive"
    sha256sum "$archive" >"$archive.sha256"
)
mv -nT "$work/$archive" "$output/$archive"
test ! -e "$work/$archive"
mv -nT "$work/$archive.sha256" "$output/$archive.sha256"
test ! -e "$work/$archive.sha256"
printf '%s\n' "$output/$archive" "$output/$archive.sha256"
