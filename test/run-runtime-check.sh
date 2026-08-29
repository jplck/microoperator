#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
base="${XDG_CACHE_HOME:-$HOME/.cache}/microorchestrator"
assets="$base/assets"
cache="$base/runtime-check"
kernel="$assets/vmlinux-6.1.155"
squashfs="$assets/ubuntu-24.04.squashfs"
rootfs="$cache/runtime.ext4"
service=""
receiver=""

cleanup() {
  [[ -z "$service" ]] || sudo systemctl kill --kill-whom=all --signal=SIGKILL "$service" 2>/dev/null || true
  [[ -z "$receiver" ]] || sudo kill "$receiver" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$assets" "$cache"
[[ -f "$kernel" ]] || curl -fL --retry 3 -o "$kernel" \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155
[[ -f "$squashfs" ]] || curl -fL --retry 3 -o "$squashfs" \
  https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/ubuntu-24.04.squashfs
printf '%s  %s\n' \
  e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2 "$kernel" \
  68321e0482baeb3844dafe8a6b08a6902401a7afc41fbfd8c3d9ea08aadd244f "$squashfs" |
  sha256sum -c -

(
  cd "$root"
  RUSTFLAGS='-C target-feature=+crt-static' cargo build --release \
    --bin guest-harness --bin vsock-check
)
install -m 0755 "$root/target/release/guest-harness" "$cache/guest-harness"
sudo rm -rf "$cache/rootfs-tree"
unsquashfs -q -d "$cache/rootfs-tree" "$squashfs"
sudo install -m 0755 "$cache/guest-harness" "$cache/rootfs-tree/guest-harness"
sudo chown -R root:root "$cache/rootfs-tree"
rm -f "$rootfs"
truncate -s 1G "$rootfs"
sudo mkfs.ext4 -q -d "$cache/rootfs-tree" -F "$rootfs"

run_vm() {
  id="$1"
  cid="$2"
  expect_outbound="$3"
  mode="$4"
  nonce="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  jail_root="/var/lib/microorchestrator/jailer/firecracker/$id/root"
  service="microorchestrator-$id.service"

  cat >"$cache/config.json" <<EOF
{
  "boot-source": {
    "kernel_image_path": "/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off init=/guest-harness check.nonce=$nonce check.expect_outbound=$expect_outbound"
  },
  "drives": [{
    "drive_id": "rootfs",
    "path_on_host": "/rootfs.ext4",
    "is_root_device": true,
    "is_read_only": true
  }],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 128
  },
  "vsock": {
    "guest_cid": $cid,
    "uds_path": "/vsock.sock"
  }
}
EOF

  sudo systemctl stop "$service" 2>/dev/null || true
  sudo rm -rf "/var/lib/microorchestrator/jailer/firecracker/$id"
  sudo install -d -m 0755 "$jail_root"
  sudo install -m 0444 "$kernel" "$jail_root/vmlinux"
  sudo install -m 0444 "$rootfs" "$jail_root/rootfs.ext4"
  sudo install -m 0444 "$cache/config.json" "$jail_root/config.json"

  sudo "$root/target/release/vsock-check" "$jail_root" "$nonce" "$mode" &
  receiver=$!
  sudo systemd-run --quiet --collect --unit "$service" \
    --property MemoryMax=256M \
    --property TasksMax=64 \
    --property CPUQuota=100% \
    /usr/local/bin/jailer \
      --id "$id" \
      --exec-file /usr/local/bin/firecracker \
      --uid 65534 \
      --gid 65534 \
      --chroot-base-dir /var/lib/microorchestrator/jailer \
      --cgroup-version 2 \
      --new-pid-ns \
      --resource-limit no-file=256 \
      --resource-limit fsize=1073741824 \
      -- \
      --config-file /config.json

  wait "$receiver"
  receiver=""
  sudo systemctl kill --kill-whom=all --signal=SIGKILL "$service" 2>/dev/null || true
  while systemctl is-active --quiet "$service"; do
    sleep 0.05
  done
  service=""
  echo "$id passed"
}

run_vm runtime-primary 3 true primary
run_vm runtime-isolated 4 false isolated
echo "jailed hello-world and bidirectional vsock passed"
