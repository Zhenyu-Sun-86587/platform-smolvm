#!/usr/bin/env bash
set -euo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
umask 077

base=/opt/sandbox-platform
etc=/etc/sandbox-platform
data=/var/lib/sandbox-platform
runtime_data=/var/lib/smolvm-platform-runtime
images=/var/lib/smolvm-platform-images
api_socket=/run/smolvm-platform/api.sock
unitdir=/etc/systemd/system
mode=install
manifest=
config=
password=
runtime_asset=
rollback=
rollback_version=
reuse=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --manifest) manifest=$2; shift 2 ;;
        --config) config=$2; shift 2 ;;
        --owner-password-file) password=$2; shift 2 ;;
        --runtime-archive) runtime_asset=$2; shift 2 ;;
        --backup) rollback=$2; shift 2 ;;
        --reuse-data) reuse=1; shift ;;
        --plan) mode=plan; shift ;;
        --upgrade) mode=upgrade; shift ;;
        --rollback) mode=rollback; rollback_version=$2; shift 2 ;;
        --uninstall) mode=uninstall; shift ;;
        *) echo "Unknown argument $1" >&2; exit 2 ;;
    esac
done
fail() { echo "$*" >&2; exit 1; }

wait_platform() {
    local pid before
    before=$(systemctl show -p MainPID --value platformd.service)
    [[ $before =~ ^[1-9][0-9]*$ ]] || return 1
    for _ in 1 2 3 4 5 6; do
        sleep 0.5
        pid=$(systemctl show -p MainPID --value platformd.service)
        [[ $pid == "$before" ]] && systemctl is-active --quiet platformd.service || return 1
        if ss -H -ltnp | grep -F "pid=$pid," >/dev/null; then
            return 0
        fi
    done
    return 1
}

if [[ $mode == uninstall ]]; then
    [[ $EUID == 0 && -f $base/.installed ]] || fail "Run as root against a managed installation"
    exec 9>"$base/.release.lock"
    flock -n 9 || fail "Another release operation is active"
    systemctl disable --now platformd.service
    for installed in "$unitdir/platformd.service" /usr/local/bin/sandboxctl "$base/current"; do
        if [[ -e $installed || -L $installed ]]; then unlink "$installed"; fi
    done
    systemctl daemon-reload
    echo "Platform service removed. Runtime, release assets, and platform data are retained."
    exit
fi

if [[ $mode == rollback ]]; then
    [[ $EUID == 0 && -f $base/.installed ]] || fail "Run as root against a managed installation"
    [[ $rollback_version =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9]+)*$ ]] || fail "Invalid rollback version"
    current=$(readlink -f "$base/current")
    target=$base/releases/$rollback_version
    [[ -f $target/release-manifest.json && $current != "$target" ]] || fail "Choose a different installed release"
    [[ $(jq -er '.schema_version' "$current/release-manifest.json") == $(jq -er '.schema_version' "$target/release-manifest.json") ]] || fail "Rollback would cross registry schema versions"
    [[ $(jq -er '.runtime_version' "$current/release-manifest.json") == $(jq -er '.runtime_version' "$target/release-manifest.json") ]] || fail "Rollback would cross runtime versions"
    for binary in platformd sandboxctl; do
        expected=$(jq -er --arg member "bin/$binary" '.files[] | select(.path == $member) | .sha256' "$target/release-manifest.json")
        [[ $(sha256sum "$target/$binary" | cut -d ' ' -f 1) == "$expected" ]] || fail "Rollback binary checksum mismatch"
    done
    "$target/platformd" -check-config -config "$etc/config.yaml"
    exec 9>"$base/.release.lock"
    flock -n 9 || fail "Another release operation is active"
    work=$(mktemp -d "$base/.rollback.XXXXXX")
    trap 'rm -rf -- "$work"' EXIT
    systemctl stop platformd.service
    ln -s "$target" "$work/target"
    mv -Tf "$work/target" "$base/current"
    if ! systemctl start platformd.service || ! wait_platform; then
        systemctl stop platformd.service
        ln -s "$current" "$work/current"
        mv -Tf "$work/current" "$base/current"
        systemctl start platformd.service && wait_platform || fail "Original release is selected but requires recovery"
        fail "Rollback startup failed. The original release is serving again."
    fi
    exit
fi

[[ -n $manifest ]] || fail "Provide --manifest RELEASE/release-manifest.json"
manifest=$(realpath -e "$manifest")
src=$(dirname -- "$manifest")
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || fail "This release requires Linux x86_64"
version=$(jq -er '.version' "$manifest")
schema=$(jq -er '.schema_version' "$manifest")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9]+)*$ && $schema =~ ^[0-9]+$ ]] || fail "Invalid release version or schema"
jq -e '.format == 1 and .architecture == "amd64" and .runtime_version == "1.37.1"' "$manifest" >/dev/null
file_list=(bin/platformd bin/sandboxctl runtime-manifest.json systemd/platformd.service
    systemd/smolvm-serve.service systemd/smolvm-status.socket systemd/smolvm-status@.service
    systemd/grant-runtime-socket systemd/read-runtime-status install.sh config.example.yaml
    managed-template.tar templates.json)
[[ $(jq -er '.files | length' "$manifest") -eq ${#file_list[@]} ]] || fail "Invalid manifest file count"
for member in "${file_list[@]}"; do
    expected=$(jq -er --arg member "$member" '.files[] | select(.path == $member) | .sha256' "$manifest")
    [[ $expected =~ ^[a-f0-9]{64}$ && -f $src/$member && ! -L $src/$member ]] || fail "Invalid release file $member"
    actual=$(sha256sum -- "$src/$member" | cut -d ' ' -f 1)
    [[ $expected == "$actual" ]] || fail "Checksum mismatch for $member"
done
runtime_manifest=$src/runtime-manifest.json
asset=smolvm-1.37.1-linux-x86_64.tar.gz
runtime_sha=f8d5d1aaa198c7b9e676fa4365998e22fc9051a2199b208741be5f38fef5e404
[[ $(jq -er '.asset' "$runtime_manifest") == "$asset" && $(jq -er '.sha256' "$runtime_manifest") == "$runtime_sha" ]] || fail "Runtime asset differs from release pin"
template_hash=$(sha256sum "$src/managed-template.tar" | cut -d ' ' -f 1)
template_name=debian-managed-${template_hash:0:12}.tar
jq -e --arg digest "sha256:$template_hash" --arg name "$template_name"     'length == 1 and .[0].image_digest == $digest and .[0].spec.local_image == $name' "$src/templates.json" >/dev/null
if [[ -n $config ]]; then
    "$src/bin/platformd" -check-config -config "$config"
fi
[[ -c /dev/kvm && -r /sys/fs/cgroup/cgroup.controllers && -d /run/systemd/system ]] || fail "KVM, cgroup v2, and systemd are required"
for tool in busctl curl e2fsck mkfs.ext4 python3 sha256sum ss tar zstd; do
    command -v "$tool" >/dev/null || fail "Missing host command $tool"
done

if [[ $mode == plan ]]; then
    jq -n --arg version "$version" --argjson schema "$schema" --arg runtime "1.37.1"         '{version:$version,schema_version:$schema,runtime_version:$runtime,release_checksums:"verified"}'
    exit
fi
[[ $EUID == 0 ]] || fail "Run as root"
if [[ $mode == install ]]; then
    [[ -n $config && -n $password && -f $password && -s $password ]] || fail "Provide --config and --owner-password-file"
    [[ ! -e $base/current && ! -e $unitdir/platformd.service && ! -e /usr/local/bin/sandboxctl ]] || fail "Platform installation target already in use"
    if [[ $reuse == 0 ]]; then
        [[ ! -e $unitdir/smolvm-serve.service && ! -e $api_socket ]] || fail "Runtime service or socket already exists"
        [[ -z $(ss -H -ltn 'sport = :10081') ]] || fail "Runtime rollout port 10081 is in use"
    fi
    if [[ -e $data || -e $runtime_data || -e $images || -e $etc ]]; then
        [[ $reuse == 1 && -f $base/.installed ]] || fail "Existing platform paths require a managed data-preserving reinstall"
    fi
    available=$(df -PB1 /var/lib | awk 'NR==2 {print $4}')
    [[ $available -ge 2147483648 ]] || fail "At least 2 GiB of free host disk space is required"
else
    [[ -f $base/.installed && -n $rollback && -d $rollback ]] || fail "Existing managed installation and pre-upgrade backup required"
    [[ -e $base/current/release-manifest.json ]] || fail "Current release manifest missing"
    previous_schema=$(jq -er '.schema_version' "$base/current/release-manifest.json")
    [[ $schema -ge $previous_schema ]] || fail "Release would downgrade the registry schema"
    "$src/bin/platformd" -check-config -config "$etc/config.yaml"
fi

mkdir -p "$base"
exec 9>"$base/.release.lock"
flock -n 9 || fail "Another release operation is active"
release_dir=$base/releases/$version
[[ ! -e $release_dir ]] || fail "Release $version already installed"
work=$(mktemp -d "$base/.release.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
runtime_dir=$base/runtime/1.37.1/bin
if [[ ! -x $runtime_dir/smolvm ]]; then
    if [[ -z $runtime_asset ]]; then
        runtime_asset=$work/$asset
        curl --proto '=https' --tlsv1.2 --fail --location --retry 2             "https://github.com/smol-machines/smolvm/releases/download/v1.37.1/$asset" -o "$runtime_asset"
    fi
    [[ $(sha256sum "$runtime_asset" | cut -d ' ' -f 1) == "$runtime_sha" ]] || fail "Official runtime archive checksum mismatch"
    mkdir "$work/runtime"
    tar -xzf "$runtime_asset" --no-same-owner -C "$work/runtime"
    runtime_install=$work/runtime/smolvm-1.37.1-linux-x86_64
else
    runtime_install=$runtime_dir
fi
(
    cd "$runtime_install"
    sha256sum -c --strict checksums.txt
    jq -r '.files | to_entries[] | "\(.value)  \(.key)"' "$runtime_manifest" | sha256sum -c --strict
    [[ $(./smolvm --version) == "smolvm 1.37.1" ]]
)
if [[ $runtime_install != "$runtime_dir" ]]; then
    install -d -m 0755 "$(dirname -- "$runtime_dir")"
    chmod 0755 "$runtime_install"
    mv -nT "$runtime_install" "$runtime_dir"
    [[ ! -e $runtime_install ]] || fail "Runtime installation target already in use"
fi

if [[ $mode == install ]]; then
    if ! getent group sandbox-control >/dev/null; then groupadd --system sandbox-control; fi
    if ! id sandbox-platform >/dev/null 2>&1; then
        useradd --system --user-group --home-dir "$data" --shell /usr/sbin/nologin sandbox-platform
    fi
    [[ $(id -u sandbox-platform) != 0 ]] || fail "Platform service user must be non-root"
    grep -Eq '^sandbox-control:[^:]*:[0-9]+:$' <(getent group sandbox-control) || fail "sandbox-control must be dedicated to the systemd service"
    install -d -o sandbox-platform -g sandbox-platform -m 0700 "$data"
    install -d -o root -g root -m 0755 "$runtime_data"
    install -d -o root -g sandbox-platform -m 0750 "$etc"
    if [[ $(realpath -m "$config") != "$etc/config.yaml" ]]; then
        [[ ! -e $etc/config.yaml ]] || cmp -s "$config" "$etc/config.yaml" || fail "Existing config differs"
        install -o root -g sandbox-platform -m 0640 "$config" "$etc/config.yaml"
    fi
    if [[ $(realpath -m "$password") != "$etc/owner-password" ]]; then
        [[ ! -e $etc/owner-password ]] || cmp -s "$password" "$etc/owner-password" || fail "Existing owner credential differs"
        install -o root -g root -m 0600 "$password" "$etc/owner-password"
    fi
fi
install -d -o root -g root -m 0755 "$base/libexec" "$base/releases" "$images"
chmod 0755 "$base"
install -d -m 0755 "$work/new"
install -m 0755 "$src/bin/platformd" "$src/bin/sandboxctl" "$work/new/"
install -m 0644 "$manifest" "$runtime_manifest" "$src/templates.json" "$work/new/"
if [[ -e $images/$template_name ]]; then
    cmp -s "$src/managed-template.tar" "$images/$template_name" || fail "Existing template differs from the verified release"
else
    install -m 0644 "$src/managed-template.tar" "$work/$template_name"
    mv -nT "$work/$template_name" "$images/$template_name"
    [[ ! -e $work/$template_name ]] || fail "Template installation target already in use"
fi
mv -nT "$work/new" "$release_dir"
[[ ! -e $work/new ]] || fail "Release installation target already in use"

previous=
if [[ $mode == upgrade ]]; then
    previous=$(readlink -f "$base/current")
    systemctl stop platformd.service
fi
ln -s "$release_dir" "$work/current"
mv -Tf "$work/current" "$base/current"
if [[ $mode == install ]]; then
    install -m 0755 "$src/systemd/grant-runtime-socket" "$src/systemd/read-runtime-status" "$base/libexec/"
    install -m 0755 "$src/install.sh" "$base/libexec/install-release"
    install -m 0644 "$src/systemd/smolvm-serve.service" "$src/systemd/smolvm-status.socket" "$src/systemd/smolvm-status@.service" "$unitdir/"
    install -m 0644 "$src/systemd/platformd.service" "$unitdir/platformd.service"
    ln -s "$base/current/sandboxctl" /usr/local/bin/sandboxctl
    install -m 0600 /dev/null "$base/.installed"
    systemctl daemon-reload
    systemctl enable --now smolvm-serve.service smolvm-status.socket platformd.service
else
    if ! systemctl start platformd.service || ! wait_platform; then
        systemctl stop platformd.service
        if [[ $schema == "$previous_schema" ]]; then
            ln -s "$previous" "$work/previous"
            mv -Tf "$work/previous" "$base/current"
            systemctl start platformd.service && wait_platform || fail "The original release is selected but requires recovery. Backup: $rollback"
        fi
        fail "Upgrade startup failed. The pre-upgrade backup is retained at $rollback"
    fi
fi
systemctl is-active --quiet smolvm-serve.service
wait_platform || fail "Platform service did not bind its management listener"
