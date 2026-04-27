//go:build mage

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Modern-kernel VM targets — orchestrate a QEMU/KVM guest on the bench
// host (msr1) so we can exercise io_uring features (multishot_recv,
// send_zc, fixed_files) on a kernel that actually supports them. The
// host's CIX-vendor 6.6.10 kernel rejects all three at probe time,
// capping celeris-iouring at the host's pessimistic configuration; a
// 6.10+ guest gives the engine its full feature surface.
//
// The whole VM lifecycle lives on the remote host. We only need ssh
// access — no kernel changes, no container daemon, no host package
// drift beyond `qemu-system-arm + qemu-utils + cloud-image-utils`.
// Tear-down deletes the qcow2 + seed iso so a re-Up rebuilds clean.
//
// Configurable via env (defaults shown):
//   CELERIS_MODERN_VM_SSH        mini@msr1            — host running qemu
//   CELERIS_MODERN_VM_IMAGE_URL  Ubuntu 24.04 arm64   — cloud image
//   CELERIS_MODERN_VM_NAME       celeris-modernvm     — VM identity
//   CELERIS_MODERN_VM_CPUS       8                    — vCPU count
//   CELERIS_MODERN_VM_MEM_MB     16384                — RAM (MB)
//   CELERIS_MODERN_VM_DISK_GB    32                   — disk size
//   CELERIS_MODERN_VM_SSH_PORT   2222                 — host:guest:22 fwd

// modernVMConfig captures the env-overridable parameters.
type modernVMConfig struct {
	sshHost  string // user@host for ssh into the bench host
	imageURL string // cloud image to base the disk on
	name     string // VM identity (also used for state dir)
	cpus     string
	memMB    string
	diskGB   string
	sshPort  string // forwarded host port → VM 22
	stateDir string // remote dir on bench host for VM artifacts
}

func loadModernVMConfig() modernVMConfig {
	c := modernVMConfig{
		sshHost:  envOrDefault("CELERIS_MODERN_VM_SSH", "mini@msr1"),
		imageURL: envOrDefault("CELERIS_MODERN_VM_IMAGE_URL", "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"),
		name:     envOrDefault("CELERIS_MODERN_VM_NAME", "celeris-modernvm"),
		cpus:     envOrDefault("CELERIS_MODERN_VM_CPUS", "8"),
		memMB:    envOrDefault("CELERIS_MODERN_VM_MEM_MB", "16384"),
		diskGB:   envOrDefault("CELERIS_MODERN_VM_DISK_GB", "32"),
		sshPort:  envOrDefault("CELERIS_MODERN_VM_SSH_PORT", "2222"),
	}
	c.stateDir = "$HOME/.celeris-modernvm/" + c.name
	return c
}

// modernVMRemote runs a bash script on the bench host (msr1) over ssh
// and streams stdout/stderr through. Returns the exit error.
func modernVMRemote(sshHost, script string) error {
	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=accept-new", sshHost, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// modernVMRemoteOutput is like modernVMRemote but returns combined output.
func modernVMRemoteOutput(sshHost, script string) (string, error) {
	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=accept-new", sshHost, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ModernVMUp creates (if missing) and starts the modern-kernel KVM
// guest on the bench host. Idempotent: a second Up against a running
// VM is a no-op past readiness probing.
//
// First-boot timeline (~60-120s on msr1):
//  1. ensure qemu/cloud-image-utils/genisoimage installed
//  2. download cloud image (~700 MB) into state dir if missing
//  3. generate ed25519 key + cloud-init seed iso
//  4. clone+resize disk from cloud image
//  5. launch qemu-system-aarch64 with KVM accel, daemonized
//  6. poll port 2222 until cloud-init finishes provisioning ssh
//  7. install Go inside the guest (one-shot)
//
// Subsequent boots skip 1-3 and start qemu directly.
func ModernVMUp() error {
	c := loadModernVMConfig()
	script := `set -euo pipefail

STATE="` + c.stateDir + `"
NAME="` + c.name + `"
IMAGE_URL="` + c.imageURL + `"
CPUS="` + c.cpus + `"
MEM_MB="` + c.memMB + `"
DISK_GB="` + c.diskGB + `"
SSH_PORT="` + c.sshPort + `"

mkdir -p "$STATE"
cd "$STATE"

# 1. Deps. Install once; subsequent runs are no-ops.
need_pkgs=()
for pkg in qemu-system-arm qemu-utils cloud-image-utils genisoimage qemu-efi-aarch64; do
  if ! dpkg -s "$pkg" >/dev/null 2>&1; then need_pkgs+=("$pkg"); fi
done
if [ "${#need_pkgs[@]}" -gt 0 ]; then
  echo "[modernvm] installing missing packages: ${need_pkgs[*]}"
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${need_pkgs[@]}"
fi

# 2. Cloud image (cached).
IMG_BASE="$(basename "$IMAGE_URL")"
if [ ! -f "$IMG_BASE" ]; then
  echo "[modernvm] downloading $IMAGE_URL"
  curl -fsSL -o "$IMG_BASE.tmp" "$IMAGE_URL"
  mv "$IMG_BASE.tmp" "$IMG_BASE"
fi

# 3. SSH key (one-time per VM identity).
if [ ! -f id_ed25519 ]; then
  ssh-keygen -t ed25519 -N "" -f id_ed25519 -C "modernvm@$NAME" >/dev/null
fi

# 3b. cloud-init user-data + seed iso (one-time).
if [ ! -f seed.iso ]; then
  cat > user-data <<USERDATA
#cloud-config
hostname: $NAME
users:
  - default
  - name: bench
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat id_ed25519.pub)
ssh_pwauth: false
package_update: true
packages:
  - git
  - curl
  - build-essential
  - tcpdump
runcmd:
  - systemctl enable --now ssh
  # Pull a recent Go toolchain straight from go.dev so we are not at the
  # mercy of distro packaging — the bench needs Go 1.26+ to match host.
  - curl -fsSL https://go.dev/dl/go1.26.1.linux-arm64.tar.gz -o /tmp/go.tar.gz
  - tar -C /usr/local -xzf /tmp/go.tar.gz
  - rm /tmp/go.tar.gz
  - echo 'export PATH=\$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
  - chmod 0644 /etc/profile.d/go.sh
USERDATA

  cat > meta-data <<METADATA
instance-id: $NAME
local-hostname: $NAME
METADATA

  cloud-localds seed.iso user-data meta-data
fi

# 4. Disk: thin clone + resize from cloud image.
if [ ! -f disk.qcow2 ]; then
  qemu-img create -f qcow2 -F qcow2 -b "$(realpath "$IMG_BASE")" disk.qcow2 "${DISK_GB}G"
fi

# 5. Already running? (PID file + alive check).
if [ -f vm.pid ] && kill -0 "$(cat vm.pid)" 2>/dev/null; then
  echo "[modernvm] VM already running (pid $(cat vm.pid))"
else
  echo "[modernvm] starting qemu (smp=$CPUS, mem=${MEM_MB}M, ssh=$SSH_PORT)"
  qemu-system-aarch64 \
    -name "$NAME" \
    -machine virt -accel kvm -cpu host \
    -smp "$CPUS" -m "$MEM_MB" \
    -bios /usr/share/qemu-efi-aarch64/QEMU_EFI.fd \
    -drive if=virtio,file=disk.qcow2,format=qcow2 \
    -drive if=virtio,file=seed.iso,format=raw,readonly=on \
    -netdev user,id=net0,hostfwd=tcp:127.0.0.1:${SSH_PORT}-:22 \
    -device virtio-net-device,netdev=net0 \
    -display none \
    -serial file:vm.log \
    -daemonize -pidfile vm.pid
fi

# 6. Wait for cloud-init to finish + ssh ready.
echo "[modernvm] waiting for ssh on 127.0.0.1:$SSH_PORT (cloud-init may take 60-120s on first boot)"
for i in $(seq 1 60); do
  if ssh -i id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
       -o ConnectTimeout=2 -p "$SSH_PORT" bench@127.0.0.1 \
       'cloud-init status --wait >/dev/null 2>&1 || true; uname -r' >/dev/null 2>&1; then
    K=$(ssh -i id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
         -p "$SSH_PORT" bench@127.0.0.1 'uname -r')
    echo "[modernvm] up. guest kernel: $K"
    echo "[modernvm] ssh: ssh -i $STATE/id_ed25519 -p $SSH_PORT bench@127.0.0.1  (from msr1)"
    exit 0
  fi
  sleep 4
done
echo "[modernvm] ERROR: ssh not ready after 240s. Tail of vm.log:"
tail -50 vm.log || true
exit 1
`
	return modernVMRemote(c.sshHost, script)
}

// ModernVMDown stops the running guest and removes the disk + seed
// iso. Cached cloud image and ssh key persist so a follow-up Up is
// fast (no re-download, no key churn).
func ModernVMDown() error {
	c := loadModernVMConfig()
	script := `set -euo pipefail

STATE="` + c.stateDir + `"
if [ ! -d "$STATE" ]; then echo "[modernvm] no state dir, nothing to do"; exit 0; fi
cd "$STATE"

if [ -f vm.pid ]; then
  PID=$(cat vm.pid)
  if kill -0 "$PID" 2>/dev/null; then
    echo "[modernvm] sending SIGTERM to qemu (pid $PID)"
    kill -TERM "$PID" || true
    for i in $(seq 1 20); do
      kill -0 "$PID" 2>/dev/null || break
      sleep 1
    done
    if kill -0 "$PID" 2>/dev/null; then
      echo "[modernvm] qemu still alive, SIGKILL"
      kill -KILL "$PID" || true
    fi
  fi
  rm -f vm.pid
fi

# Drop the per-VM artifacts; keep the (~700 MB) cloud image cached
# and the ssh key so re-Up is fast.
rm -f disk.qcow2 seed.iso user-data meta-data vm.log
echo "[modernvm] down. cached: $STATE/{cloud-image,id_ed25519}"
`
	return modernVMRemote(c.sshHost, script)
}

// ModernVMStatus prints VM state, kernel, and io_uring feature
// detection from inside the guest. Cheap — used for sanity-checking
// before kicking off a long bench run.
func ModernVMStatus() error {
	c := loadModernVMConfig()
	script := `set -euo pipefail

STATE="` + c.stateDir + `"
SSH_PORT="` + c.sshPort + `"

if [ ! -d "$STATE" ]; then echo "state dir missing — run mage modernVMUp"; exit 1; fi
cd "$STATE"

if [ ! -f vm.pid ] || ! kill -0 "$(cat vm.pid)" 2>/dev/null; then
  echo "VM: stopped"
  exit 0
fi

PID=$(cat vm.pid)
RSS_KB=$(awk '/VmRSS/ {print $2}' /proc/$PID/status 2>/dev/null || echo "?")
UPTIME=$(ps -o etime= -p "$PID" 2>/dev/null | tr -d ' ' || echo "?")
echo "VM: running pid=$PID rss=${RSS_KB}KB up=${UPTIME}"

ssh -i id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -p "$SSH_PORT" bench@127.0.0.1 \
    'echo "guest kernel: $(uname -r)"; echo; echo "io_uring features (sysfs):"; ls /sys/class/io_uring 2>/dev/null || echo "  (sysfs probe absent — fall back to runtime probe via celeris)"; echo; echo "cpu cores: $(nproc)"; echo "mem: $(free -h | awk "/Mem:/ {print \$2}")"' \
  || echo "ssh probe failed"
`
	return modernVMRemote(c.sshHost, script)
}

// ModernVMSSH opens an interactive ssh session into the running VM.
// Convenience for poking around. Inherits stdin/stdout/stderr.
func ModernVMSSH() error {
	c := loadModernVMConfig()
	script := `set -euo pipefail
STATE="` + c.stateDir + `"
SSH_PORT="` + c.sshPort + `"
exec ssh -t -i "$STATE/id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
     -p "$SSH_PORT" bench@127.0.0.1
`
	cmd := exec.Command("ssh", "-t", "-o", "StrictHostKeyChecking=accept-new", c.sshHost, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ModernVMBench rsyncs the current celeris worktree into the VM,
// builds the perfmatrix runner inside the guest, and runs an isolated
// celeris-iouring get-simple bench with multishot_recv, send_zc, and
// fixed_files all opt-in via env. Output is piped back to the local
// terminal. Fast iteration after the first sync (rsync is delta-only).
func ModernVMBench() error {
	c := loadModernVMConfig()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Step 1: rsync the source tree into the VM via the host.
	// We can't rsync directly to the guest (port forward is at the host),
	// so push to the host's state dir first, then push to the guest from
	// inside the host with `scp -P 2222`.
	rsyncToHost := exec.Command("rsync", "-az", "--delete",
		"--exclude=.git", "--exclude=results", "--exclude=node_modules",
		"--exclude=*.test", "--exclude=test/perfmatrix/cmd/runner/default.pgo",
		cwd+"/", c.sshHost+":"+strings.ReplaceAll(c.stateDir, "$HOME", "$HOME")+"/celeris-src/")
	rsyncToHost.Stdout = os.Stdout
	rsyncToHost.Stderr = os.Stderr
	if err := rsyncToHost.Run(); err != nil {
		return fmt.Errorf("rsync to host: %w", err)
	}

	// Step 2 inside the bench host: push to guest, build, run bench.
	script := `set -euo pipefail

STATE="` + c.stateDir + `"
SSH_PORT="` + c.sshPort + `"

if [ ! -f "$STATE/vm.pid" ] || ! kill -0 "$(cat $STATE/vm.pid)" 2>/dev/null; then
  echo "VM not running — start with 'mage modernVMUp' first"
  exit 1
fi

SSH_OPTS=(-i "$STATE/id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p "$SSH_PORT")
SCP_OPTS=(-i "$STATE/id_ed25519" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P "$SSH_PORT")

# Sync source from host state dir to guest. rsync via ssh tunnel.
echo "[modernvm-bench] syncing source to guest"
ssh "${SSH_OPTS[@]}" bench@127.0.0.1 'mkdir -p ~/celeris && rm -rf ~/celeris/.git || true'
rsync -az -e "ssh -i $STATE/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p $SSH_PORT" \
  "$STATE/celeris-src/" bench@127.0.0.1:~/celeris/

# Build + run inside guest. /usr/local/go/bin must be in PATH (set by cloud-init in /etc/profile.d/go.sh).
echo "[modernvm-bench] building + running inside guest"
ssh "${SSH_OPTS[@]}" bench@127.0.0.1 'bash -lc "
  set -e
  cd ~/celeris
  /usr/local/go/bin/go version
  cd test/perfmatrix
  /usr/local/go/bin/go get github.com/goceleris/loadgen >/dev/null
  echo
  echo \"=== HOST KERNEL: \$(uname -r) ===\"
  echo
  echo \"=== iouring (multishot OFF — default) ===\"
  ENG=iouring PROTO=h1 ASYNC=0 SCENARIO=get-simple DUR=20 \
    /usr/local/go/bin/go run ./cmd/perfprofile -out /tmp/vmprof-default 2>&1 | grep -E \"rps|tier|multishot|fixed|send_zc|kernel\" || true
  echo
  echo \"=== iouring (multishot ON via env) ===\"
  CELERIS_IOURING_MULTISHOT_RECV=1 ENG=iouring PROTO=h1 ASYNC=0 SCENARIO=get-simple DUR=20 \
    /usr/local/go/bin/go run ./cmd/perfprofile -out /tmp/vmprof-mshot 2>&1 | grep -E \"rps|tier|multishot|fixed|send_zc|kernel\" || true
  echo
  echo \"=== epoll ===\"
  ENG=epoll PROTO=h1 ASYNC=0 SCENARIO=get-simple DUR=20 \
    /usr/local/go/bin/go run ./cmd/perfprofile -out /tmp/vmprof-epoll 2>&1 | grep -E \"rps|kernel\" || true
"'
`
	return modernVMRemote(c.sshHost, script)
}

// silence-unused-warnings sentinel: errors imported above.
var _ = errors.New
