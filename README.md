# Btrfs CSI Driver

[![Go Version](https://img.shields.io/github/go-mod/go-version/mporrato/btrfs-csi)](https://github.com/mporrato/btrfs-csi)
[![License: Unlicense](https://img.shields.io/badge/License-Unlicense-green.svg)](LICENSE)

A Kubernetes CSI storage driver for node-local storage on single- or multi-node clusters that exposes btrfs features—instant volumes, snapshots, clones, and true quota enforcement—directly to Kubernetes workloads.

## Why btrfs?

Btrfs is a copy-on-write filesystem with built-in volume management. This driver exposes those capabilities to Kubernetes, giving you features that traditional storage drivers can't match:

### 1. Instant Volumes
**What btrfs does:** Creates subvolumes instantly—no formatting, no zeroing, no allocation delay.
**Kubernetes mapping:** Every PVC becomes a btrfs subvolume.
**Why it matters:** A 100 GiB volume provisions in milliseconds, not minutes. No waiting for disk allocation or filesystem initialization.

### 2. Copy-on-Write Control
**What btrfs does:** CoW tracks changes by copying data blocks before modification. You can disable it per-subvolume with `chattr +C` (nodatacow).
**Kubernetes mapping:** The `cow` StorageClass parameter controls this behavior.
**Why it matters:** Databases (PostgreSQL, MySQL) and VMs perform better with CoW disabled—they do their own caching and suffer write amplification from CoW. General workloads benefit from CoW's efficiency.

### 3. Instant Snapshots
**What btrfs does:** Creates read-only snapshots in zero time—no data copy, just metadata.
**Kubernetes mapping:** `VolumeSnapshot` objects create crash-consistent, read-only btrfs snapshots.
**Why it matters:** Back up a 500 GiB database instantly. No performance impact, no staging period.

### 4. Instant Clones
**What btrfs does:** Writable snapshots (clones) share data with the source until modified (CoW efficiency).
**Kubernetes mapping:** PVC-to-PVC cloning via `dataSource`.
**Why it matters:** Clone a database for testing in seconds. Multiple clones share the same underlying data until writes diverge.

### 5. True Quota Enforcement
**What btrfs does:** Qgroups enforce hard capacity limits at the subvolume level.
**Kubernetes mapping:** PVC capacity requests become qgroup limits.
**Why it matters:** A 10 GiB PVC cannot exceed 10 GiB—period. Not advisory, not best-effort. Hard enforcement prevents runaway workloads from consuming all storage.

### 6. Online Expansion
**What btrfs does:** Resize qgroup limits instantly; the filesystem expands online.
**Kubernetes mapping:** Patch a PVC's capacity; the volume grows without downtime.
**Why it matters:** No unmount, no resize operations, no service interruption. Just update the PVC and use the extra space immediately.

Expansion is performed by kubelet on the volume's node. Mounted volumes expand online; unused volumes finish expanding when next mounted by a pod.

### 7. Cross-Filesystem Clones
**What btrfs does:** Send/receive streams enable cloning across different btrfs filesystems.
**Kubernetes mapping:** Clone PVCs across different storage pools.
**Why it matters:** Copy data between physical disks or backup locations efficiently—only changed blocks transfer.

### 8. Auto-Discovered Storage Pools
**What btrfs does:** Each btrfs mount becomes a named storage pool.
**Kubernetes mapping:** Mount a btrfs filesystem under `/var/lib/btrfs-csi/<pool-name>`; the driver finds it automatically.
**Why it matters:** No ConfigMap, no manual pool registration. Add a disk, mount it, and it's ready to use.

## Prerequisites

Before deploying the driver, ensure the following:

- **btrfs-progs installed** on the node (`btrfs --version` should work)
- **A btrfs filesystem mounted** under `/var/lib/btrfs-csi/<pool-name>`
- **Root/privileged access** (the driver runs as privileged)
- **Shared mount propagation** enabled: `mount --make-rshared /` (if not already default)

## Quick Start

Get a PVC running in a few commands:

```bash
# 1. Mount a btrfs filesystem and enable quotas
mkdir -p /var/lib/btrfs-csi/default
mount /dev/sdX /var/lib/btrfs-csi/default
btrfs quota enable /var/lib/btrfs-csi/default

# 2. Deploy the driver (with VolumeSnapshot support)
make deploy OVERLAY=snapshot    # CRDs + controller
make deploy                     # Driver + snapshot support
make deploy OVERLAY=storageclass  # Default StorageClass

# 3. Create a PVC
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  storageClassName: btrfs
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
EOF

# 4. Use it in a pod
kubectl run test --image=alpine --rm -it --restart=Never \
  --overrides='{"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"my-data"}}],"containers":[{"name":"test","image":"alpine","volumeMounts":[{"name":"data","mountPath":"/data"}]}]}}'
```

## Features with Examples

### Instant Volumes

Create a PVC and it's ready immediately—no provisioning delay:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fast-volume
spec:
  storageClassName: btrfs
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: use-volume
spec:
  containers:
  - name: app
    image: alpine
    command: ["sleep", "3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: fast-volume
```

### Copy-on-Write Control

Use `cow: "false"` for databases or workloads with heavy random writes:

```yaml
# Default: CoW enabled (good for general workloads)
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: btrfs-cow
provisioner: btrfs.csi.local
volumeBindingMode: WaitForFirstConsumer  # ensures pod is scheduled before binding
allowVolumeExpansion: true
parameters:
  cow: "true"  # default, can omit
---
# No CoW: Best for databases, VMs, random-write workloads
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: btrfs-nocow
provisioner: btrfs.csi.local
volumeBindingMode: WaitForFirstConsumer  # ensures pod is scheduled before binding
allowVolumeExpansion: true
parameters:
  cow: "false"  # disables CoW with chattr +C
```

**Topology:** The driver uses the topology key `topology.btrfs.csi.local/node`. With `WaitForFirstConsumer`, only the provisioner on the first consumer's selected node creates the volume. The resulting PV has required node affinity, so every subsequent consumer is scheduled on that same node.

**When to use each:**
- `cow: "true"` (default): Web servers, log aggregation, general applications—benefits from CoW efficiency
- `cow: "false"`: PostgreSQL, MySQL, Elasticsearch, VMs—workloads that do their own caching and suffer from CoW write amplification

### Compression and Per-PVC Overrides

StorageClasses can also set `compression: "zstd"`, `"zlib"`, `"lzo"`, `"none"`,
or `"inherit"` (the default). Compression levels such as `zstd:5` are rejected:
btrfs does not support persistent per-directory compression levels.

PVC annotations override the StorageClass:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: compressed-data
  annotations:
    btrfs.csi.local/cow: "true"
    btrfs.csi.local/compression: "zstd"
spec:
  storageClassName: btrfs
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
```

Both annotations may be edited after binding. The owning node reconciles changes
at startup and every 30 seconds. Removing an annotation uses the **current**
StorageClass default, so deleting and recreating a StorageClass with changed
parameters also updates its existing PVCs. Invalid updates retain the last valid
settings and produce a `VolumeOptionsUpdateFailed` warning Event; API failures or
missing StorageClasses never reset settings.

Changes affect **only the subvolume root's defaults**, not existing files or
subdirectories. New children inherit their parent directory's settings; files
created inside an existing subdirectory continue to inherit that subdirectory's
old settings. No recursive attribute changes, data rewriting, or pool remounts
are performed. `inherit` clears the root's explicit compression property and
compression flags, including legacy flag-only compression;
`none` explicitly prohibits compression.

Effective `cow: "false"` disables compression inherited from the StorageClass.
It also disables checksumming for affected new files. An explicit PVC request
for enabled compression while COW is disabled is rejected; enable COW explicitly
when overriding a NOCOW class with compression.

Clones and restores retain the source root's COW/compression defaults at creation,
even if the destination class or annotations differ. A subsequent settings edit
applies the destination defaults to the root, without converting inherited data.
Cross-pool transfers explicitly normalize the received root before publication.
This does not recursively restore per-file inode flags omitted by btrfs receive
or rewrite received file data.
Existing volumes are safely adopted through their bound PV and PVC UID: their
root settings are preserved unless explicit annotations request a change.
Same-name replacement claims cannot change retained volumes.

PVC integration uses in-cluster credentials. `--pvc-options=auto` enables it in
Kubernetes and leaves standalone CSI usage StorageClass-only. Use `enabled` to
require credentials or `disabled` to opt out. The shipped RBAC already permits
the required PVC, PV, StorageClass, and Event operations.

Autodefrag, independent nodatasum, and RAID profiles are filesystem-wide rather
than per-subvolume settings and are not exposed here. Deduplication requires an
external out-of-band scanning tool; there is no native per-subvolume dedupe mode.

### Multi-Node Clusters

Prepare btrfs pools under `/var/lib/btrfs-csi/<pool-name>` on each storage node, then use the normal deployment overlays. Pool paths and names are local to each node; use the same pool name on nodes that should serve the same StorageClass. Nodes without valid pools cannot provision volumes. Keep `--nodeid` set to the Kubernetes node name, as the DaemonSet does.

By default, the DaemonSet targets every eligible node: prepare pools on **all** of them. In a mixed storage/non-storage cluster, label only prepared storage nodes:

```bash
kubectl label node worker-1 storage.btrfs.csi.local/enabled=true
```

Add this strategic-merge patch to your deployment's kustomize overlay, retaining any existing node selectors:

```yaml
patches:
  - target:
      kind: DaemonSet
      name: btrfs-csi-driver
    patch: |-
      apiVersion: apps/v1
      kind: DaemonSet
      metadata:
        name: btrfs-csi-driver
      spec:
        template:
          spec:
            nodeSelector:
              storage.btrfs.csi.local/enabled: "true"
```

Use this administrator-managed label, not the topology label that the driver registers after startup. Without targeting, unprepared nodes run crashing driver pods; their unavailability can also consume the DaemonSet's rolling-update budget and stall upgrades. Label and prepare a new storage node before making it eligible, and keep the selector in your overlay for subsequent deployments.

Use `WaitForFirstConsumer` StorageClasses and let the scheduler place pods. To select a storage node for a new volume, use a pod `nodeSelector`, not `spec.nodeName`, which bypasses scheduling.

Volumes are not replicated or moved between nodes. If a storage node is unavailable, consumers of its volumes remain pending until that node returns. Deletion also waits for the owning node; do not remove PV finalizers as a substitute for deleting the on-disk data.

Snapshots, restores, and clones remain on the source node, including transfers between pools on that node. For a clone or snapshot restore, select the source node in the consuming pod:

```yaml
spec:
  nodeSelector:
    topology.btrfs.csi.local/node: worker-1
```

Find the source node in the source PV's `.spec.nodeAffinity`. For restores after the source PVC has been deleted, record the node from the snapshot content's `snapshot.storage.kubernetes.io/managed-by` label. A consumer selecting another node cannot restore or clone the local data.

The bundled snapshot controller enables distributed snapshotting. If your cluster manages its own snapshot controller, configure it with `--enable-distributed-snapshotting` and grant its service account `get`, `list`, and `watch` on nodes. Each driver snapshotter uses `--node-deployment` and `NODE_NAME`; these settings must be enabled together.

#### Upgrading Existing Installations

No volume handles, pool paths, or state files change. Before adding storage nodes, inspect every existing PV for this driver: the older deployment did not explicitly enable the provisioner's topology feature, so some PVs may have no node affinity. Such PVs are not safe to use in a multi-node cluster.

PVs that already have the correct required node affinity need no changes. For an affected PV, back up its manifest and data, stop its consumers, set its reclaim policy to `Retain`, and use Kubernetes' retained-volume recovery procedure to rebind a replacement PV with the same CSI volume handle and required affinity to the original node. Do not add nodes or resume consumers until every existing PV is correctly pinned. Do not delete the backing subvolume or pool state during recovery.

Pause new provisioning while upgrading the driver. After every old provisioner has stopped and the updated DaemonSet has finished rolling out, remove only this driver's legacy centrally managed capacity reports:

```bash
kubectl -n btrfs-csi rollout status daemonset/btrfs-csi-driver --timeout=180s
kubectl -n btrfs-csi delete csistoragecapacities \
  -l 'csi.storage.k8s.io/drivername=btrfs.csi.local,csi.storage.k8s.io/managed-by=external-provisioner'
kubectl -n btrfs-csi get csistoragecapacities \
  -l 'csi.storage.k8s.io/drivername=btrfs.csi.local'
```

Use your deployment's namespace if different. Verify that the remaining reports use node-specific `csi.storage.k8s.io/managed-by=external-provisioner-<node-name>` identities and that each storage node reports capacity for its StorageClasses before resuming provisioning. The old `external-provisioner` reports survive a rolling upgrade because their DaemonSet owner survives; the new per-node provisioners do not manage them. A stale optimistic report can cause the scheduler to select storage that no longer has enough capacity. Do not delete other drivers' reports or the current node-specific reports.

Before switching snapshotters to distributed mode, label existing VolumeSnapshotContents for this driver with the node holding their data:

```bash
kubectl label volumesnapshotcontent <content-name> \
  snapshot.storage.kubernetes.io/managed-by=<source-node>
```

Apply this to retained and imported snapshots too; derive the node from the source PV or your storage inventory, never guess. Existing contents are not automatically backfilled by the upstream controller. New dynamic snapshots receive the label automatically. Configure the snapshot controller before rolling out the updated driver.

### Snapshots

Create instant, crash-consistent backups and restore them:

```yaml
# Create a snapshot
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: db-backup
spec:
  volumeSnapshotClassName: btrfs
  source:
    persistentVolumeClaimName: database-pvc
---
# Restore from snapshot to a new PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: db-restored
spec:
  storageClassName: btrfs
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
  dataSource:
    name: db-backup
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
```

### Cloning

Clone a PVC for testing, staging, or data migration:

```yaml
# Clone an existing PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: production-db-clone
spec:
  storageClassName: btrfs
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi  # can be same or larger than source
  dataSource:
    name: production-db
    kind: PersistentVolumeClaim
```

The clone shares data with the source until you modify it—extremely space-efficient.

### Volume Expansion

Grow a volume online without downtime:

```yaml
# Original PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: expandable-volume
spec:
  storageClassName: btrfs
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
```

```bash
# Expand to 20 GiB
kubectl patch pvc expandable-volume --patch '{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}'

# Verify (no unmount or restart needed)
kubectl get pvc expandable-volume
```

The pod continues running and can immediately use the extra space.

### Multi-Pool Storage

Use multiple btrfs filesystems for different storage tiers:

```bash
# On the node: mount multiple btrfs filesystems
mkdir -p /var/lib/btrfs-csi/default
mkdir -p /var/lib/btrfs-csi/fast-ssd
mount /dev/sda1 /var/lib/btrfs-csi/default
mount /dev/nvme0n1 /var/lib/btrfs-csi/fast-ssd
btrfs quota enable /var/lib/btrfs-csi/default
btrfs quota enable /var/lib/btrfs-csi/fast-ssd
```

```yaml
# StorageClasses for each pool
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: btrfs-default
provisioner: btrfs.csi.local
volumeBindingMode: WaitForFirstConsumer
parameters:
  pool: default
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: btrfs-fast
provisioner: btrfs.csi.local
volumeBindingMode: WaitForFirstConsumer
parameters:
  pool: fast-ssd
  cow: "false"  # disable CoW for maximum SSD performance
```

```yaml
# Use the fast pool for databases
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fast-db
spec:
  storageClassName: btrfs-fast
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 50Gi
```

## StorageClass Parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `pool` | (auto) | Pool name. Auto-selects the sole pool, or the pool named `default`. If multiple pools exist and none is named `default`, volume creation fails — explicitly specify the pool. |
| `cow` | `"true"` | Set to `"false"` to disable copy-on-write (nodatacow). Use for databases and VMs. |
| `compression` | `"inherit"` | Root compression default: `inherit`, `none`, `zstd`, `zlib`, or `lzo`. No per-subvolume levels. Suppressed by `cow: "false"`. |

## Deployment

The driver deploys with `--enable-capacity` by default, which publishes `CSIStorageCapacity` objects to the API server. This allows the Kubernetes scheduler to track available storage capacity per node and prevent over-provisioning.

### Kustomize Overlays

| Overlay | Description |
|---------|-------------|
| `snapshot` | VolumeSnapshot CRDs + controller (apply first) |
| `minimal` | Driver only: no StorageClass, no VolumeSnapshot support |
| `default` | Driver + VolumeSnapshot support (no classes) |
| `storageclass` | Standalone default `btrfs` StorageClass |
| `volumesnapshotclass` | Standalone default `btrfs` VolumeSnapshotClass |
| `dev` | Full dev setup with secondary pool for e2e testing |

### Deploy Commands

Standard deployment flow:

```bash
# 1. Install VolumeSnapshot CRDs and controller
make deploy OVERLAY=snapshot

# 2. Deploy the driver with snapshot support
make deploy

# 3. Apply default classes (optional, or bring your own)
make deploy OVERLAY=storageclass
make deploy OVERLAY=volumesnapshotclass
```

Minimal deployment (volumes only, no snapshots):

```bash
make deploy OVERLAY=minimal
```

### Custom kubelet path

The manifests default to `/var/lib/kubelet`. Set `KUBELET_DIR` for distributions with different paths:

| Distribution | Kubelet path |
|---|---|
| Standard / minikube / kind | `/var/lib/kubelet` (default) |
| k0s | `/var/lib/k0s/kubelet` |
| k3s | `/var/lib/rancher/k3s/agent/kubelet` |

Check your kubelet root:

```bash
ps aux | grep kubelet | grep -o '\--root-dir=[^ ]*'
```

Deploy with custom path:

```bash
make deploy OVERLAY=snapshot KUBELET_DIR=/var/lib/k0s/kubelet
make deploy KUBELET_DIR=/var/lib/k0s/kubelet
make deploy OVERLAY=minimal KUBELET_DIR=/var/lib/k0s/kubelet
```

### Node preparation

**Mount propagation:** The driver requires shared mount propagation for bind mounts to work correctly.

Check current mode:
```bash
findmnt -o TARGET,PROPAGATION /
```

If it shows `private`, enable shared propagation:
```bash
mount --make-rshared /
```

To persist on Alpine Linux (which defaults to `private`):
```bash
echo "mount --make-rshared /" > /etc/local.d/shared-mounts.start
chmod +x /etc/local.d/shared-mounts.start
```

**Pool setup:** Each pool is a btrfs mountpoint:
```bash
mkdir -p /var/lib/btrfs-csi/default
mount /dev/sdX /var/lib/btrfs-csi/default
btrfs quota enable /var/lib/btrfs-csi/default
```

The driver auto-discovers pools every 30 seconds—no restart needed.

## Configuration

### Driver Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--endpoint` | `unix:///csi/csi.sock` | CSI endpoint (Unix socket) |
| `--nodeid` | (auto-generated UUID) | Node ID for topology |
| `--pools-dir` | `/var/lib/btrfs-csi` | Base directory for pool discovery |
| `--kubelet-dir` | `/var/lib/kubelet` | Kubelet root directory for path validation |
| `--enable-capacity` | `true` | Publish `CSIStorageCapacity` objects to prevent over-provisioning |
| `--version` | | Print version and exit |

## Troubleshooting

### Driver not starting

**Symptom:** Driver pod crashes or fails to become ready.

**Check:**
```bash
# Verify btrfs-progs is installed
btrfs --version

# Check pools-dir exists and has btrfs mounts
ls -la /var/lib/btrfs-csi/
mount | grep btrfs

# Verify quota is enabled
btrfs quota show /var/lib/btrfs-csi/default

# Check driver logs
kubectl logs -n btrfs-csi -l app=btrfs-csi-driver
```

**Fix:**
- Ensure at least one btrfs filesystem is mounted under `--pools-dir`
- Enable quotas: `btrfs quota enable <pool-path>`
- Verify the CSI socket directory is writable

### Volume creation fails

**Symptom:** PVC stays in `Pending` state with provisioning errors.

**Check:**
```bash
# Verify pool exists and is mounted
btrfs filesystem df /var/lib/btrfs-csi/default

# Check available space
btrfs filesystem usage /var/lib/btrfs-csi/default

# Verify StorageClass references correct pool
kubectl get storageclass btrfs -o yaml
```

**Fix:**
- Ensure the pool name in StorageClass matches a subdirectory under `--pools-dir`
- Check available space on the btrfs filesystem
- Verify quota is enabled on the pool

### Mount errors

**Symptom:** Pod fails to start with mount errors.

**Check:**
```bash
# Verify mount propagation
findmnt -o TARGET,PROPAGATION /

# Check kubelet logs
journalctl -u kubelet | grep -i mount

# Verify driver has privileged access
kubectl get daemonset -n btrfs-csi -o yaml | grep -A5 securityContext
```

**Fix:**
- Enable shared mount propagation: `mount --make-rshared /`
- Ensure the driver runs with privileged security context (default in manifests)

### Capacity/quota issues

**Symptom:** Volume creation fails with capacity errors, or writes fail despite available space.

**Check:**
```bash
# Show qgroup limits
btrfs qgroup show /var/lib/btrfs-csi/default

# Check actual usage vs limit
btrfs filesystem usage /var/lib/btrfs-csi/default
```

**Fix:**
- Qgroups enforce hard limits—a 10 GiB PVC cannot exceed 10 GiB even if the filesystem has space
- Delete unused volumes/snapshots to free quota
- Expand the PVC if more space is needed

### Volume health

The driver does not report an explicit health condition (the CSI `VolumeCondition` API was removed from the spec in v1.13.0). Volume health is implied by standard channels:

- `NodeGetVolumeStats` returns `NotFound` for a deleted/unmounted volume or `Internal` on qgroup errors; kubelet surfaces these as pod events.
- A qgroup-limited volume that is full reports `Available: 0` in stats; writes fail with `ENOSPC` inside the pod.
- Check recent events with `kubectl describe pod <name>` and `kubectl get events`.

## Development

### Building from Source

```bash
make build   # Build the binary
make image   # Build the container image
```

### Testing

```bash
# Run unit tests
make test

# Run integration tests (requires root + btrfs)
make test-integration

# Create a local minikube cluster for e2e testing
make minikube-up

# Run end-to-end tests
make minikube-e2e

# Tear down the test cluster
make minikube-down
```

For two-node coverage, start minikube with `--nodes=2`, prepare both nodes with `bash scripts/ci/setup.sh`, and run `make minikube-e2e-multinode`. This checks ownership, pod replacement, usable capacity after expansion, snapshots, local clones/restores, quotas, capacity reporting, legacy capacity cleanup, restart, and deletion on both nodes. Run this only on an isolated test cluster: the mixed-node case temporarily unmounts the second node's pools to verify storage-node targeting and rolling updates, then restores the pools, labels, and selector.

### Project Structure

```
btrfs-csi/
├── cmd/btrfs-csi-driver/    # Entry point
├── pkg/
│   ├── driver/              # CSI gRPC services
│   ├── btrfs/               # btrfs CLI wrapper
│   └── state/               # JSON-backed metadata store
├── deploy/
│   ├── base/                # Core manifests
│   ├── components/          # Composable kustomize components
│   └── overlays/            # Environment-specific configurations
└── scripts/                 # Cluster setup and test scripts
```

## License

Public domain (Unlicense). See [LICENSE](LICENSE) for details.
