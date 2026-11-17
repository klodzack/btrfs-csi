package driver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"github.com/mporrato/btrfs-csi/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/klog/v2"
)

// PVCClient is the Kubernetes surface required for PVC option resolution.
type PVCClient interface {
	GetPVC(context.Context, string, string) (*corev1.PersistentVolumeClaim, error)
	GetPV(context.Context, string) (*corev1.PersistentVolume, error)
	ListPVs(context.Context) ([]corev1.PersistentVolume, error)
	GetStorageClass(context.Context, string) (*storagev1.StorageClass, error)
	Warning(context.Context, *corev1.PersistentVolumeClaim, string) error
}

type optionWarning struct {
	message string
	at      time.Time
}

// SetPVCClient configures PVC support before serving requests.
func (d *Driver) SetPVCClient(client PVCClient) {
	d.pvcClient = client
}

func (d *Driver) pvcClassParameters(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (map[string]string, error) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil, nil
	}
	class, err := d.pvcClient.GetStorageClass(ctx, *pvc.Spec.StorageClassName)
	if err != nil {
		return nil, fmt.Errorf("get StorageClass: %w", err)
	}
	if class == nil || class.Provisioner != driverName {
		return nil, fmt.Errorf("StorageClass is not owned by %s", driverName)
	}
	return class.Parameters, nil
}

func (d *Driver) pvcCreationSettings(ctx context.Context, req *csi.CreateVolumeRequest,
	requested btrfs.RootSettings) (*state.VolumeOptions, error) {
	opts := &state.VolumeOptions{Request: requested, InputFingerprint: settingsFingerprint(req.Parameters, nil)}
	if d.pvcClient == nil {
		opts.Creation = requested
		return opts, nil
	}
	name := req.Parameters["csi.storage.k8s.io/pvc/name"]
	namespace := req.Parameters["csi.storage.k8s.io/pvc/namespace"]
	if name == "" && namespace == "" {
		opts.Creation = requested
		return opts, nil
	}
	if name == "" || namespace == "" {
		return nil, status.Error(codes.InvalidArgument, "PVC name and namespace metadata must both be supplied")
	}
	pvc, err := d.pvcClient.GetPVC(ctx, namespace, name)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "get provisioning PVC: %v", err)
	}
	if pvc == nil || pvc.UID == "" || pvc.DeletionTimestamp != nil || pvc.Name != name || pvc.Namespace != namespace {
		return nil, status.Error(codes.FailedPrecondition, "provisioning PVC identity is missing or being deleted")
	}
	params, err := d.pvcClassParameters(ctx, pvc)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "resolve provisioning defaults: %v", err)
	}
	opts.Creation, err = resolveRootSettings(params, pvc.Annotations)
	if err != nil {
		return nil, err
	}
	opts.PVC = state.PVCReference{Namespace: namespace, Name: name, UID: string(pvc.UID)}
	opts.InputFingerprint = settingsFingerprint(params, pvc.Annotations)
	return opts, nil
}

// RunPVCOptions reconciles immediately and then periodically until cancellation.
func (d *Driver) RunPVCOptions(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.ReconcilePVCOptions(ctx); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "PVC option reconciliation failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReconcilePVCOptions applies valid changes only to locally stored, bound claims.
func (d *Driver) ReconcilePVCOptions(ctx context.Context) error {
	if d.pvcClient == nil {
		return nil
	}
	var failures []error
	var legacyPVs []corev1.PersistentVolume
	var listed bool
	for _, observed := range d.store.ListVolumes() {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref := state.PVCReference{}
		if observed.Options != nil {
			ref = observed.Options.PVC
		}
		if ref.UID == "" {
			if !listed {
				var err error
				legacyPVs, err = d.pvcClient.ListPVs(ctx)
				if err != nil {
					return fmt.Errorf("discover legacy bindings: %w", err)
				}
				listed = true
			}
			ref = legacyPVCReference(legacyPVs, observed.ID)
			if ref.UID == "" {
				continue
			}
		}
		if err := d.reconcilePVCVolume(ctx, observed, ref); err != nil {
			failures = append(failures, fmt.Errorf("volume %s: %w", observed.ID, err))
		}
	}
	return errors.Join(failures...)
}

func legacyPVCReference(pvs []corev1.PersistentVolume, volumeID string) state.PVCReference {
	var ref state.PVCReference
	for i := range pvs {
		pv := &pvs[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driverName || pv.Spec.CSI.VolumeHandle != volumeID {
			continue
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID == "" || pv.DeletionTimestamp != nil {
			continue
		}
		if ref.UID != "" {
			// Ambiguous handles must never be adopted.
			return state.PVCReference{}
		}
		ref = state.PVCReference{
			Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name, UID: string(pv.Spec.ClaimRef.UID),
		}
	}
	return ref
}

func (d *Driver) boundPVC(ctx context.Context, ref state.PVCReference,
	volumeID string) (*corev1.PersistentVolumeClaim, error) {
	pvc, err := d.pvcClient.GetPVC(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("get PVC: %w", err)
	}
	if pvc == nil || string(pvc.UID) != ref.UID || pvc.Namespace != ref.Namespace || pvc.Name != ref.Name {
		return nil, fmt.Errorf("PVC identity changed")
	}
	if pvc.DeletionTimestamp != nil || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return nil, nil
	}
	pv, err := d.pvcClient.GetPV(ctx, pvc.Spec.VolumeName)
	if err != nil {
		return nil, fmt.Errorf("get bound PV: %w", err)
	}
	if !matchesPVCBinding(pv, pvc, volumeID) {
		return nil, fmt.Errorf("PV binding does not match stored PVC and local volume")
	}
	return pvc, nil
}

func matchesPVCBinding(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim, volumeID string) bool {
	if pv == nil || pv.DeletionTimestamp != nil || pv.Name != pvc.Spec.VolumeName || pv.Spec.CSI == nil {
		return false
	}
	ref := pv.Spec.ClaimRef
	return pv.Spec.CSI.Driver == driverName && pv.Spec.CSI.VolumeHandle == volumeID &&
		ref != nil && ref.UID == pvc.UID && ref.Namespace == pvc.Namespace && ref.Name == pvc.Name
}

func (d *Driver) reconcilePVCVolume(ctx context.Context, observed *state.Volume, ref state.PVCReference) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pvc, err := d.boundPVC(ctx, ref, observed.ID)
	if err != nil || pvc == nil {
		return err
	}
	params, err := d.pvcClassParameters(ctx, pvc)
	if err != nil {
		return err
	}
	desired, err := resolveRootSettings(params, pvc.Annotations)
	if err != nil {
		d.warnPVC(ctx, pvc, err)
		return err
	}
	if err := d.applyPVCVolume(ctx, observed, pvc, params, desired); err != nil {
		d.warnPVC(ctx, pvc, err)
		return err
	}
	return nil
}

func (d *Driver) applyPVCVolume(ctx context.Context, observed *state.Volume,
	pvc *corev1.PersistentVolumeClaim, params map[string]string, desired btrfs.RootSettings) error {
	ref := state.PVCReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: string(pvc.UID)}
	fingerprint := settingsFingerprint(params, pvc.Annotations)
	d.controllerMu.Lock()
	defer d.controllerMu.Unlock()
	vol, ok := d.store.GetVolume(observed.ID)
	if !ok || vol.BasePath != observed.BasePath {
		return nil
	}
	if !reflect.DeepEqual(vol.Options, observed.Options) {
		return nil // Another operation advanced this volume while the API was being read.
	}
	if vol.Options != nil && vol.Options.PVC.UID != "" && vol.Options.PVC != ref {
		return fmt.Errorf("stored PVC identity changed")
	}
	if vol.Options == nil || vol.Options.PVC.UID == "" {
		if err := d.adoptPVCVolume(ctx, vol, pvc, params, desired); err != nil {
			return err
		}
	}
	if vol.Options.InputFingerprint != fingerprint {
		vol.Options.Pending = &desired
		vol.Options.InputFingerprint = fingerprint
	}
	if vol.Options.Pending == nil {
		return nil
	}
	// A failed FileStore write may advance memory, so retry persistence before every mutation.
	if err := d.store.SaveVolume(vol); err != nil {
		return fmt.Errorf("save pending root settings: %w", err)
	}
	if err := d.manager.ApplyRootSettings(ctx, vol.Path(), *vol.Options.Pending); err != nil {
		return err
	}
	vol.Options.Applied = *vol.Options.Pending
	vol.Nodatacow = vol.Options.Applied.Nodatacow
	vol.Options.Pending = nil
	if err := d.store.SaveVolume(vol); err != nil {
		return fmt.Errorf("save applied root settings: %w", err)
	}
	return nil
}

func (d *Driver) adoptPVCVolume(ctx context.Context, vol *state.Volume,
	pvc *corev1.PersistentVolumeClaim, params map[string]string, desired btrfs.RootSettings) error {
	actual, err := d.manager.GetRootSettings(ctx, vol.Path())
	if err != nil {
		return err
	}
	if vol.Options == nil {
		request := btrfs.RootSettings{Nodatacow: vol.Nodatacow, Compression: "inherit"}
		if request.Nodatacow {
			request.Compression = "none"
		}
		vol.Options = &state.VolumeOptions{Legacy: true, Request: request, Creation: actual}
	}
	vol.Options.PVC = state.PVCReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: string(pvc.UID)}
	vol.Options.Applied = actual
	vol.Nodatacow = actual.Nodatacow
	vol.Options.InputFingerprint = settingsFingerprint(params, pvc.Annotations)
	_, cowOverride := pvc.Annotations[cowAnnotation]
	_, compressionOverride := pvc.Annotations[compressionAnnotation]
	if cowOverride || compressionOverride {
		vol.Options.Pending = &desired
	}
	if err := d.store.SaveVolume(vol); err != nil {
		return fmt.Errorf("save adopted PVC identity: %w", err)
	}
	return nil
}

func (d *Driver) warnPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, err error) {
	d.optionWarningsMu.Lock()
	if d.optionWarnings == nil || len(d.optionWarnings) > 1024 {
		d.optionWarnings = make(map[string]optionWarning)
	}
	key := string(pvc.UID)
	last := d.optionWarnings[key]
	if last.message == err.Error() && time.Since(last.at) < 5*time.Minute {
		d.optionWarningsMu.Unlock()
		return
	}
	d.optionWarnings[key] = optionWarning{message: err.Error(), at: time.Now()}
	d.optionWarningsMu.Unlock()
	if eventErr := d.pvcClient.Warning(ctx, pvc, err.Error()); eventErr != nil {
		klog.ErrorS(eventErr, "Record PVC options warning", "namespace", pvc.Namespace, "pvc", pvc.Name)
	}
}
