# shellcheck shell=bash
# Sourced by e2e.sh; uses its isolated namespace, helpers, and node executor.

options_volume_path() {
    local pv id
    pv=$(${K} get pvc "$1" -n "${NAMESPACE}" -o jsonpath='{.spec.volumeName}')
    id=$(${K} get pv "${pv}" -o jsonpath='{.spec.csi.volumeHandle}')
    [ -n "${id}" ] || return 1
    printf '%s/volumes/%s' "${BTRFS_MOUNT_1}" "${id}"
}

options_compression() {
    ${EXEC} "sudo btrfs property get '$1' compression" 2>/dev/null | tr -d '\r'
}

options_nocow() {
    local attrs
    attrs=$(${EXEC} "sudo lsattr -d '$1'" 2>/dev/null | awk '{print $1}')
    [ -n "${attrs}" ] || return 1
    case "${attrs}" in
        *C*) printf true ;;
        *) printf false ;;
    esac
}

assert_options() {
    if wait_until 90 "$1" "$2"; then
        pass "$3"
    else
        fail "$3"
    fi
}

options_storageclass() {
    ${K} apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $1
  labels:
    btrfs-csi-options-e2e: ${NAMESPACE}
provisioner: btrfs.csi.local
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
parameters:
  pool: default
  cow: "true"
  compression: $2
EOF
}

section "StorageClass compression and live PVC root defaults"

_options_class="btrfs-options-${NAMESPACE}"
options_storageclass "${_options_class}" zstd

apply_pvc e2e-options-default "${_options_class}" 64Mi
bind_pvc e2e-options-default "Compression-default PVC bound"
_options_default_path=$(options_volume_path e2e-options-default)
assert_options "options_compression '${_options_default_path}'" "compression=zstd" \
    "StorageClass compression default applied"

apply_pvc e2e-options-override "${_options_class}" 64Mi
${K} annotate pvc e2e-options-override -n "${NAMESPACE}" btrfs.csi.local/cow=false
bind_pvc e2e-options-override "PVC COW override bound"
_options_override_path=$(options_volume_path e2e-options-override)
assert_options "options_nocow '${_options_override_path}'" true "PVC COW override applied"
run_pod e2e-options-old-data e2e-options-override \
    "mkdir /data/existing && echo existing-data > /data/existing/file"

${K} annotate pvc e2e-options-override -n "${NAMESPACE}" --overwrite \
    btrfs.csi.local/cow=true btrfs.csi.local/compression=lzo
assert_options "options_compression '${_options_override_path}'" "compression=lzo" \
    "Live compression annotation applied"
assert_options "options_nocow '${_options_override_path}'" false "Live COW annotation applied"
assert_options "options_nocow '${_options_override_path}/existing'" true \
    "Existing subdirectory attributes were not rewritten"
assert_options "options_compression '${_options_default_path}'" "compression=zstd" \
    "Neighboring PVC compression was not changed"

${K} annotate pvc e2e-options-override -n "${NAMESPACE}" \
    btrfs.csi.local/cow- btrfs.csi.local/compression-
assert_options "options_compression '${_options_override_path}'" "compression=zstd" \
    "Removing annotations restores current class defaults"
assert_pod_succeeds e2e-options-read-data e2e-options-override \
    "grep existing-data /data/existing/file" "Live root updates preserve existing data"

${K} delete storageclass "${_options_class}"
options_storageclass "${_options_class}" lzo
assert_options "options_compression '${_options_default_path}'" "compression=lzo" \
    "Replacing the StorageClass updates existing PVC defaults"
assert_options "options_compression '${_options_override_path}'" "compression=lzo" \
    "Removed annotations follow the replacement StorageClass"

log "Restarting the isolated test driver to verify persisted options..."
${K} rollout restart daemonset/btrfs-csi-driver -n btrfs-csi
${K} rollout status daemonset/btrfs-csi-driver -n btrfs-csi --timeout=180s
assert_options "options_compression '${_options_override_path}'" "compression=lzo" \
    "Root settings survive driver restart"
${K} annotate pvc e2e-options-override -n "${NAMESPACE}" btrfs.csi.local/compression=zlib
assert_options "options_compression '${_options_override_path}'" "compression=zlib" \
    "Reconciliation resumes after restart"

delete_resources pvc/e2e-options-default pvc/e2e-options-override
${K} delete storageclass "${_options_class}"
