#!/usr/bin/env bash
set -euo pipefail

repo=$(cd -- "$(dirname -- "$0")/../.." && pwd)
gates="$repo/packaging/managed-template-gates.json"
if [[ $# == 2 ]]; then
    version=$1
    output=$(realpath -m "$2")
    basename=$(jq -r '.default_archive.basename' "$gates")
    default_digest=$(jq -r '.default_archive.sha256' "$gates")
    template=$(realpath -m "${SANDBOX_MANAGED_TEMPLATE_DIR:-/var/lib/smolvm-phase1-images}/$basename")
    if [[ ! -f "$template" || $(sha256sum "$template" | cut -d ' ' -f 1) != "$default_digest" ]]; then
        echo "Verified default template $basename is missing or has a different SHA-256. Set SANDBOX_MANAGED_TEMPLATE_DIR to its directory or supply an archive explicitly." >&2
        exit 2
    fi
elif [[ $# == 3 ]]; then
    version=$1
    template=$(realpath "$2")
    output=$(realpath -m "$3")
else
    echo "Usage: $0 VERSION [MANAGED_TEMPLATE.tar] OUTPUT_DIRECTORY" >&2
    exit 2
fi
[[ $version =~ ^[0-9]+[.][0-9]+[.][0-9]+([.-][a-zA-Z0-9]+)*$ ]]
[[ -f "$template" && ! -e "$output" ]]
work=$(mktemp -d "$(dirname -- "$output")/.sandbox-release.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
cd "$repo"
mkdir "$work/bin" "$work/systemd"
for executable in platformd sandboxctl sandbox-node; do
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags "-X sandbox-platform/internal/buildinfo.Version=$version" \
        -o "$work/bin/$executable" "./cmd/$executable"
done
cp packaging/runtime-manifest.json "$work/runtime-manifest.json"
cp packaging/systemd/{platformd.service,sandbox-node.service,sandbox-node.example.yaml,smolvm-serve.service,smolvm-status.socket,smolvm-status@.service,grant-runtime-socket,read-runtime-status} "$work/systemd/"
cp packaging/install/install.sh "$work/install.sh"
cp packaging/install/config.example.yaml "$work/config.example.yaml"
cp "$template" "$work/managed-template.tar"
cd "$work"
template_hash=$(sha256sum managed-template.tar | cut -d ' ' -f 1)
runtime_version=$(jq -r .version runtime-manifest.json)
jq -n --arg digest "$template_hash" --arg runtime "$runtime_version" --slurpfile gates "$gates" '
  ($gates[0].archives[$digest] | if .runtime_version == $runtime and .architecture == "amd64" then . else null end) as $gate
  | (($gate.catalog_version // 1)
      | if type == "number" and . >= 1 and . == floor then . else error("invalid template catalog_version") end) as $catalog_version
  | ("debian-managed-" + $digest[0:12]) as $name
  | [{
      template_id:($name + (if $catalog_version == 1 then "" else "-v" + ($catalog_version | tostring) end)),
      name:$name, version:$catalog_version, image_digest:("sha256:" + $digest),
      spec:({local_image:($name + ".tar")}
          + (if $gate == null then {} else {verification_evidence:$gate.evidence} end)),
      capabilities:(reduce ($gate.capabilities // [])[] as $name ({};
          .[$name] = {supported:true, verified_profile:$gate.verified_profile}))
    } + (if $gate == null then {} else {guest_release:"Debian 12", verified_at:$gate.verified_at} end)]
' > templates.json
members=(bin/platformd bin/sandboxctl bin/sandbox-node systemd/sandbox-node.service systemd/sandbox-node.example.yaml runtime-manifest.json systemd/platformd.service
    systemd/smolvm-serve.service systemd/smolvm-status.socket systemd/smolvm-status@.service
    systemd/grant-runtime-socket systemd/read-runtime-status install.sh config.example.yaml
    managed-template.tar templates.json)
files=$(sha256sum "${members[@]}" | jq -Rn '[inputs | capture("(?<sha256>[a-f0-9]{64})  (?<path>.+)")]')
schema=$(find "$repo/migrations" -maxdepth 1 -name '[0-9]*_*.sql' -printf '%f\n' | cut -d_ -f1 | sort -n | tail -1)
schema=$((10#$schema))
jq -n --arg version "$version" --arg runtime "$runtime_version"     --argjson schema "$schema" --argjson files "$files"     '{format:1, version:$version, schema_version:$schema, runtime_version:$runtime, architecture:"amd64", files:$files}' > release-manifest.json
mv -nT "$work" "$output"
test ! -e "$work"
