# microorchestrator

Single-host control plane for isolated AI agents. The implementation is
currently in Phase 0 feasibility work.

## Phase 0 host setup

The current development host is Ubuntu on x86_64 Linux.

### 1. Install build tools

```bash
sudo apt-get update
sudo apt-get install -y cargo rustc rustfmt curl
```

Rust 1.75 or newer is required.

### 2. Grant KVM access

`/dev/kvm` must exist and be read-write for the user running
microorchestrator:

```bash
ls -l /dev/kvm
sudo usermod -aG kvm "$USER"
```

Log out and back in to refresh group membership. For the current shell, run a
single command with the refreshed group:

```bash
sg kvm -c 'cargo run -- host-check'
```

If `/dev/kvm` does not exist, enable KVM or nested virtualization in the host
before continuing.

### 3. Install pinned Firecracker and jailer

Download the official x86_64 release and verify its published SHA-256 checksum:

```bash
version=v1.16.1
arch=x86_64
dir="$HOME/.cache/microorchestrator/firecracker-$version"
mkdir -p "$dir"
cd "$dir"

curl -fLO "https://github.com/firecracker-microvm/firecracker/releases/download/$version/firecracker-$version-$arch.tgz"
curl -fLO "https://github.com/firecracker-microvm/firecracker/releases/download/$version/firecracker-$version-$arch.tgz.sha256.txt"
sha256sum -c "firecracker-$version-$arch.tgz.sha256.txt"
tar -xzf "firecracker-$version-$arch.tgz"

sudo install -m 0755 \
  "release-$version-$arch/firecracker-$version-$arch" \
  /usr/local/bin/firecracker
sudo install -m 0755 \
  "release-$version-$arch/jailer-$version-$arch" \
  /usr/local/bin/jailer
```

### 4. Verify the host

```bash
cargo test
cargo build
target/debug/microorchestrator host-check
```

The check reports Linux and kernel versions, KVM access, cgroup v2,
Firecracker/jailer versions and hashes, AF_VSOCK support, and free disk. Every
line must report `OK` before the remaining Phase 0 spikes can run.

## Phase 0.2 jailed hello-world

The spike uses Firecracker CI's Ubuntu 24.04 guest filesystem and Linux
6.1.155 kernel. Their pinned SHA-256 hashes are checked before boot.

Install the image build tools:

```bash
sudo apt-get install -y squashfs-tools e2fsprogs
```

Run the spike:

```bash
./test/run-runtime-check.sh
```

The script builds a trusted static Rust guest init, creates a 1 GiB ext4 image,
and boots it read-only through the jailer with CPU, memory, PID,
file-descriptor, disk, and runtime bounds. It verifies:

- random nonces received over dedicated vsock Unix sockets from UID 1000;
- host-to-guest streaming through a guest loopback HTTP service;
- guest-to-host HTTP, cancellation, timeout, and 4 KiB request bounds;
- no guest internet route or network interface;
- a second VM cannot use the first VM's host-side service.

## Phase 0.4 OCI artifact spike

Install the OCI tools:

```bash
sudo apt-get install -y docker.io skopeo umoci
```

Run the spike:

```bash
./test/run-oci-check.sh
```

The check builds the upstream A2A hello-world sample at commit
`6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3`, converts it to an OCI layout,
materializes its root filesystem onto a read-only workload disk, boots it
unmodified, and fetches its A2A agent card over vsock. It reports the resolved
OCI digest, materialization time, boot time, and disk amplification.

Measured on the initial WSL2 development host:

| Result | Value |
|---|---:|
| OCI digest | `sha256:2a9865298d30e2781ddbacdf94e2784bb5c4b5a480968344a3124a39ce4604bb` |
| Materialization | 3.0 seconds |
| Boot to agent card | 2.7 seconds |
| Fixed disk amplification | 4.52x |

Direct materialization was selected. The alternative in-guest `runc` path adds
a runtime, bundle setup, namespaces, and cgroup plumbing without improving the
microVM boundary. Privileged operations are limited to OCI ownership
restoration, ext4 creation, jail setup, and guest mounts. Cleanup targets the
exact systemd unit and jail directory.

## Phase 0.5 governance spike

Install the pinned static OPA v1.20.1 binary and verify its published checksum:

```bash
version=v1.20.1
dir="$HOME/.cache/microorchestrator/opa-$version"
mkdir -p "$dir"
cd "$dir"

curl -fLO "https://github.com/open-policy-agent/opa/releases/download/$version/opa_linux_amd64_static"
curl -fLO "https://github.com/open-policy-agent/opa/releases/download/$version/opa_linux_amd64_static.sha256"
sha256sum -c opa_linux_amd64_static.sha256
sudo install -m 0755 opa_linux_amd64_static /usr/local/bin/opa
```

Run the spike:

```bash
cargo run --bin governance-check
```

It exercises allow, deny, and evaluator-timeout fail-closed behavior. Each
decision prints its trace ID and policy digest, and only the allowed request
may reach the fake upstream.
