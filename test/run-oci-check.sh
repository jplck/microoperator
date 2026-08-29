#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
base="${XDG_CACHE_HOME:-$HOME/.cache}/microorchestrator"
assets="$base/assets"
cache="$base/oci-check"
commit=6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3
tag="microorchestrator-a2a-helloworld:$commit"
kernel="$assets/vmlinux-6.1.155"
squashfs="$assets/ubuntu-24.04.squashfs"
jail_root="/var/lib/microorchestrator/jailer/firecracker/oci-check/root"
service="microorchestrator-oci-check.service"

cleanup() {
  sudo systemctl kill --kill-whom=all --signal=SIGKILL "$service" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$assets" "$cache/source"
[[ -f "$kernel" ]] || curl -fL --retry 3 -o "$kernel" \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155
[[ -f "$squashfs" ]] || curl -fL --retry 3 -o "$squashfs" \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/ubuntu-24.04.squashfs
printf '%s  %s\n' \
  e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2 "$kernel" \
  68321e0482baeb3844dafe8a6b08a6902401a7afc41fbfd8c3d9ea08aadd244f "$squashfs" |
  sha256sum -c -

if [[ ! -f "$cache/source/helloworld/requirements.txt" ]]; then
  curl -fL --retry 3 \
    "https://github.com/a2aproject/a2a-samples/archive/$commit.tar.gz" |
    tar -xz --strip-components=4 -C "$cache/source" \
      "a2a-samples-$commit/samples/python/agents/helloworld"
fi

docker build --quiet -f "$root/test/Dockerfile.a2a-helloworld" \
  -t "$tag" "$cache/source/helloworld" >/dev/null
docker save -o "$cache/image.tar" "$tag"
sudo rm -rf "$cache/oci" "$cache/bundle"
skopeo copy "docker-archive:$cache/image.tar" "oci:$cache/oci:agent" >/dev/null
digest="$(skopeo inspect --format '{{.Digest}}' "oci:$cache/oci:agent")"

start="$(date +%s%N)"
sudo umoci unpack --image "$cache/oci:agent" "$cache/bundle"
sudo chown -R root:root "$cache/bundle/rootfs"
truncate -s 512M "$cache/workload.ext4"
sudo mkfs.ext4 -q -d "$cache/bundle/rootfs" -F "$cache/workload.ext4"
materialize_ms="$((($(date +%s%N) - start) / 1000000))"
workload_hash="$(sha256sum "$cache/workload.ext4" | cut -d' ' -f1)"

(
  cd "$root"
  RUSTFLAGS='-C target-feature=+crt-static' cargo build --release \
    --bin oci-guest --bin oci-check
)
sudo rm -rf "$cache/base-rootfs"
unsquashfs -q -d "$cache/base-rootfs" "$squashfs"
sudo install -m 0755 "$root/target/release/oci-guest" "$cache/base-rootfs/oci-guest"
sudo install -d -m 0755 "$cache/base-rootfs/mnt"
sudo chown -R root:root "$cache/base-rootfs"
truncate -s 1G "$cache/base.ext4"
sudo mkfs.ext4 -q -d "$cache/base-rootfs" -F "$cache/base.ext4"

cat >"$cache/config.json" <<EOF
{
  "boot-source": {
    "kernel_image_path": "/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off init=/oci-guest"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "/base.ext4",
      "is_root_device": true,
      "is_read_only": true
    },
    {
      "drive_id": "workload",
      "path_on_host": "/workload.ext4",
      "is_root_device": false,
      "is_read_only": true
    }
  ],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 256
  },
  "vsock": {
    "guest_cid": 5,
    "uds_path": "/vsock.sock"
  }
}
EOF

sudo systemctl stop "$service" 2>/dev/null || true
sudo rm -rf /var/lib/microorchestrator/jailer/firecracker/oci-check
sudo install -d -m 0755 "$jail_root"
sudo install -m 0444 "$kernel" "$jail_root/vmlinux"
sudo install -m 0444 "$cache/base.ext4" "$jail_root/base.ext4"
sudo install -m 0444 "$cache/workload.ext4" "$jail_root/workload.ext4"
sudo install -m 0444 "$cache/config.json" "$jail_root/config.json"

boot_start="$(date +%s%N)"
sudo systemd-run --quiet --collect --unit "$service" \
  --property MemoryMax=384M \
  --property TasksMax=128 \
  --property CPUQuota=100% \
  /usr/local/bin/jailer \
    --id oci-check \
    --exec-file /usr/local/bin/firecracker \
    --uid 65534 \
    --gid 65534 \
    --chroot-base-dir /var/lib/microorchestrator/jailer \
    --cgroup-version 2 \
    --new-pid-ns \
    --resource-limit no-file=512 \
    --resource-limit fsize=1073741824 \
    -- \
    --config-file /config.json
sudo "$root/target/release/oci-check" "$jail_root/vsock.sock"
boot_ms="$((($(date +%s%N) - boot_start) / 1000000))"

sudo systemctl kill --kill-whom=all --signal=SIGKILL "$service" 2>/dev/null || true
while systemctl is-active --quiet "$service"; do sleep 0.05; done
service=""
test "$workload_hash" = "$(sha256sum "$cache/workload.ext4" | cut -d' ' -f1)"

rootfs_bytes="$(sudo du -sb "$cache/bundle/rootfs" | cut -f1)"
disk_bytes="$(stat -c %s "$cache/workload.ext4")"
printf 'image=%s\nmaterialize_ms=%s\nboot_ms=%s\ndisk_amplification=%.2fx\n' \
  "$digest" "$materialize_ms" "$boot_ms" \
  "$(awk "BEGIN { print $disk_bytes / $rootfs_bytes }")"
echo "selected=materialized read-only workload disk"
echo "rejected=in-guest runc adds a runtime, bundle setup, namespaces, and cgroup plumbing"
