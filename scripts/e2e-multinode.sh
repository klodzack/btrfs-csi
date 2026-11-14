#!/usr/bin/env bash
# Requires an isolated minikube cluster with at least two prepared storage nodes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${NAMESPACE:=btrfs-csi-multinode-e2e}"
source "${SCRIPT_DIR}/common.sh"

cleanup() {
    local rc=$?
    if [ "${rc}" -ne 0 ]; then
        ${K} get pods,pvc -n "${NAMESPACE}" -o wide || true
        ${K} get events -n "${NAMESPACE}" --sort-by=.lastTimestamp || true
        ${K} logs -n btrfs-csi -l app=btrfs-csi-driver --all-containers --tail=40 --prefix || true
    fi
    ${K} delete namespace "${NAMESPACE}" --ignore-not-found --wait=true --timeout=180s || true
}
trap cleanup EXIT

mapfile -t nodes < <(${K} get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
[ "${#nodes[@]}" -ge 2 ] || { echo "At least two nodes are required" >&2; exit 1; }
${K} create namespace "${NAMESPACE}"

create_pvc() {
    local name="$1" kind="${2:-}" source="${3:-}" size="${4:-64Mi}" datasource=""
    if [ -n "${kind}" ]; then
        datasource="  dataSource:
    kind: ${kind}
    name: ${source}"
        if [ "${kind}" = VolumeSnapshot ]; then
            datasource="${datasource}
    apiGroup: snapshot.storage.k8s.io"
        fi
    fi
    ${K} apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ${PRIMARY_STORAGECLASS}
  resources:
    requests:
      storage: ${size}
${datasource}
EOF
}

create_pod() {
    local name="$1" pvc="$2" node="${3:-}" selector=""
    if [ -n "${node}" ]; then
        selector="  nodeSelector:
    topology.btrfs.csi.local/node: ${node}"
    fi
    ${K} apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
spec:
${selector}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: consumer
      image: busybox:1.37
      command: [sh, -c, "sleep 1800"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${pvc}
EOF
}

ready() {
    ${K} wait -n "${NAMESPACE}" --for=condition=Ready "pod/$1" --timeout=180s
}

check_storage() {
    local owner="$1" id="$2" name="$3" node count=0 records
    for node in "${nodes[@]}"; do
        records=$(${MK} ssh --node="${node}" -- \
            "if sudo test -f ${BTRFS_MOUNT_1}/state.json; then sudo cat ${BTRFS_MOUNT_1}/state.json; else echo '{}'; fi")
        count=$((count + $(jq --arg name "${name}" '[.volumes[]? | select(.Name == $name)] | length' <<<"${records}")))
        if [ "${node}" = "${owner}" ]; then
            ${MK} ssh --node="${node}" -- "sudo btrfs subvolume show ${BTRFS_MOUNT_1}/volumes/${id}" >/dev/null
        else
            ${MK} ssh --node="${node}" -- "test ! -e ${BTRFS_MOUNT_1}/volumes/${id}"
        fi
    done
    [ "${count}" -eq 1 ]
}

for index in 0 1; do
    node="${nodes[index]}"
    other="${nodes[1-index]}"
    name="local-${index}"
    echo "==> Testing volume ownership on ${node}..."
    create_pvc "${name}"
    create_pod "${name}" "${name}" "${node}"
    ready "${name}"
    pv=$(${K} get pvc "${name}" -n "${NAMESPACE}" -o jsonpath='{.spec.volumeName}')
    id=$(${K} get pv "${pv}" -o jsonpath='{.spec.csi.volumeHandle}')
    ${K} get pv "${pv}" -o json | jq -e --arg node "${node}" '
        .spec.nodeAffinity.required.nodeSelectorTerms |
        length == 1 and (.[0].matchExpressions |
        length == 1 and .[0].key == "topology.btrfs.csi.local/node" and
        .[0].operator == "In" and .[0].values == [$node])' || {
            ${K} get pv "${pv}" -o yaml
            exit 1
        }
    check_storage "${node}" "${id}" "${pv}"
    ${K} exec -n "${NAMESPACE}" "${name}" -- sh -c 'echo persistent-data > /data/value'

    echo "==> Testing online expansion and quota enforcement..."
    ${K} patch pvc "${name}" -n "${NAMESPACE}" --type=merge \
        -p '{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}'
    ${K} wait -n "${NAMESPACE}" "pvc/${name}" --for=jsonpath='{.status.capacity.storage}'=128Mi --timeout=180s
    ${K} exec -n "${NAMESPACE}" "${name}" -- sh -c \
        'if dd if=/dev/zero of=/data/overflow bs=1M count=160 2>/dev/null; then exit 1; fi; rm -f /data/overflow'

    echo "==> Testing snapshot and same-node clone/restore..."
    ${K} apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
spec:
  volumeSnapshotClassName: ${PRIMARY_STORAGECLASS}
  source:
    persistentVolumeClaimName: ${name}
EOF
    ${K} wait -n "${NAMESPACE}" "volumesnapshot/${name}" --for=jsonpath='{.status.readyToUse}'=true --timeout=180s
    content=$(${K} get volumesnapshot "${name}" -n "${NAMESPACE}" -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    ${K} get volumesnapshotcontent "${content}" -o json | jq -e --arg node "${node}" \
        '.metadata.labels["snapshot.storage.kubernetes.io/managed-by"] == $node'
    for kind in PersistentVolumeClaim VolumeSnapshot; do
        copy="${name}-${kind,,}"
        create_pvc "${copy}" "${kind}" "${name}" 128Mi
        create_pod "${copy}" "${copy}" "${node}"
        ready "${copy}"
        ${K} exec -n "${NAMESPACE}" "${copy}" -- grep persistent-data /data/value
        ${K} delete pod "${copy}" -n "${NAMESPACE}" --wait=true
        ${K} delete pvc "${copy}" -n "${NAMESPACE}" --wait=true --timeout=180s
    done
    ${K} delete volumesnapshot "${name}" -n "${NAMESPACE}" --wait=true --timeout=180s

    echo "==> Testing deferred expansion and unconstrained replacement..."
    ${K} delete pod "${name}" -n "${NAMESPACE}" --wait=true
    ${K} patch pvc "${name}" -n "${NAMESPACE}" --type=merge \
        -p '{"spec":{"resources":{"requests":{"storage":"192Mi"}}}}'
    create_pod "${name}" "${name}"
    ready "${name}"
    [ "$(${K} get pod "${name}" -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}')" = "${node}" ]
    ${K} wait -n "${NAMESPACE}" "pvc/${name}" --for=jsonpath='{.status.capacity.storage}'=192Mi --timeout=180s
    ${K} exec -n "${NAMESPACE}" "${name}" -- grep persistent-data /data/value

    echo "==> Testing conflicting scheduling and driver restart..."
    create_pod "${name}-conflict" "${name}" "${other}"
    ${K} wait -n "${NAMESPACE}" "pod/${name}-conflict" \
        --for=jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}'=Unschedulable --timeout=60s
    [ -z "$(${K} get pod "${name}-conflict" -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}')" ]
    ${K} delete pod "${name}-conflict" "${name}" -n "${NAMESPACE}" --wait=true
    ${K} delete pod -n btrfs-csi -l app=btrfs-csi-driver --field-selector "spec.nodeName=${node}" --wait=true
    ${K} rollout status daemonset/btrfs-csi-driver -n btrfs-csi --timeout=180s
    create_pod "${name}" "${name}"
    ready "${name}"
    ${K} exec -n "${NAMESPACE}" "${name}" -- grep persistent-data /data/value
    check_storage "${node}" "${id}" "${pv}"
    ${K} delete pod "${name}" -n "${NAMESPACE}" --wait=true
    ${K} delete pvc "${name}" -n "${NAMESPACE}" --wait=true --timeout=180s
    ${K} wait --for=delete "pv/${pv}" --timeout=180s
    ${MK} ssh --node="${node}" -- "test ! -e ${BTRFS_MOUNT_1}/volumes/${id}"
done

echo "==> Checking per-node storage capacity..."
for node in "${nodes[@]:0:2}"; do
    ${K} get csistoragecapacities -n btrfs-csi -o json | jq -e --arg node "${node}" --arg sc "${PRIMARY_STORAGECLASS}" '
        any(.items[]; .storageClassName == $sc and
        .nodeTopology.matchLabels["topology.btrfs.csi.local/node"] == $node and
        .capacity != "0")'
done
echo "Multi-node E2E tests passed."
