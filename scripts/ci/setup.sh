#!/usr/bin/env bash
# CI-only: set up two loop-backed btrfs filesystems inside an already-running
# minikube cluster, then build and deploy the btrfs-csi driver.
#
# Expects minikube to be started externally (e.g., via medyagh/setup-minikube).
#
# Usage (from repo root):
#   bash scripts/ci/setup.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../common.sh"

if ! grep -qw btrfs /proc/filesystems; then
    echo "==> Loading btrfs kernel module on host..."
    sudo modprobe btrfs
fi

for node in $(${K} get nodes -o jsonpath='{.items[*].metadata.name}'); do
    echo "==> Installing btrfs-progs in ${node}..."
    ${MK} ssh --node="${node}" -- "sudo apt-get update -qq && sudo apt-get install -y -qq btrfs-progs"

    echo "==> Creating loop-backed btrfs filesystems on ${node}..."
    ${MK} ssh --node="${node}" -- "sudo bash -ec '
  truncate -s 512M /tmp/btrfs1.img
  truncate -s 512M /tmp/btrfs2.img
  LOOP1=\$(losetup -f)
  test -b \"\$LOOP1\" || mknod \"\$LOOP1\" b 7 \"\${LOOP1#/dev/loop}\"
  losetup \"\$LOOP1\" /tmp/btrfs1.img
  LOOP2=\$(losetup -f)
  test -b \"\$LOOP2\" || mknod \"\$LOOP2\" b 7 \"\${LOOP2#/dev/loop}\"
  losetup \"\$LOOP2\" /tmp/btrfs2.img
  mkfs.btrfs -f \"\$LOOP1\"
  mkfs.btrfs -f \"\$LOOP2\"
  mkdir -p ${BTRFS_MOUNT_1} ${BTRFS_MOUNT_2} ${RUNTIME_POOL_DIR}
  mount \"\$LOOP1\" ${BTRFS_MOUNT_1}
  mount \"\$LOOP2\" ${BTRFS_MOUNT_2}
'"
done

echo "==> Building driver image..."
_VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
${RUNTIME} build --build-arg "VERSION=${_VERSION}" -t "${IMAGE}" .

echo "==> Deploying driver..."
bash "${SCRIPT_DIR}/../deploy.sh"

echo ""
echo "CI cluster '${CLUSTER}' is ready."
