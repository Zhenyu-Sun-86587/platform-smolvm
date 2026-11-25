#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 ]]; then
    echo "Usage: $0 VERSION MANAGED_TEMPLATE.tar OUTPUT_DIRECTORY" >&2
    exit 2
fi
version=$1
template=$(realpath "$2")
output=$(realpath -m "$3")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9]+)*$ ]]
[[ -f "$template" && ! -e "$output" ]]
repo=$(cd -- "$(dirname -- "$0")/../.." && pwd)
work=$(mktemp -d "$(dirname -- "$output")/.sandbox-release.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
cd "$repo"
mkdir "$work/bin" "$work/systemd"
for executable in platformd sandboxctl; do
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -o "$work/bin/$executable" "./cmd/$executable"
done
cp packaging/runtime-manifest.json "$work/runtime-manifest.json"
cp packaging/systemd/{platformd.service,smolvm-serve.service,smolvm-status.socket,smolvm-status@.service,grant-runtime-socket,read-runtime-status} "$work/systemd/"
cp packaging/install/install.sh "$work/install.sh"
cp packaging/install/config.example.yaml "$work/config.example.yaml"
cp "$template" "$work/managed-template.tar"
cd "$work"
template_hash=$(sha256sum managed-template.tar | cut -d ' ' -f 1)
jq -n --arg digest "$template_hash" '[{
    template_id:("debian-managed-" + $digest[0:12]), name:("debian-managed-" + $digest[0:12]),
    version:1, image_digest:("sha256:" + $digest), guest_release:"Debian 12",
    spec:{local_image:("debian-managed-" + $digest[0:12] + ".tar")},
    capabilities:{}
}]' > templates.json
members=(bin/platformd bin/sandboxctl runtime-manifest.json systemd/platformd.service
    systemd/smolvm-serve.service systemd/smolvm-status.socket systemd/smolvm-status@.service
    systemd/grant-runtime-socket systemd/read-runtime-status install.sh config.example.yaml
    managed-template.tar templates.json)
files=$(sha256sum "${members[@]}" | jq -Rn '[inputs | capture("(?<sha256>[a-f0-9]{64})  (?<path>.+)")]')
schema=$(find "$repo/migrations" -maxdepth 1 -name '[0-9][0-9][0-9]_*.sql' | wc -l)
jq -n --arg version "$version" --arg runtime "$(jq -r .version runtime-manifest.json)"     --argjson schema "$schema" --argjson files "$files"     '{format:1, version:$version, schema_version:$schema, runtime_version:$runtime, architecture:"amd64", files:$files}' > release-manifest.json
mv -nT "$work" "$output"
test ! -e "$work"
