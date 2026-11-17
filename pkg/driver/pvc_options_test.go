package driver

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"github.com/mporrato/btrfs-csi/pkg/state"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type fakePVCClient struct {
	pvc      *corev1.PersistentVolumeClaim
	pv       *corev1.PersistentVolume
	class    *storagev1.StorageClass
	err      error
	warnings []string
}

func (f *fakePVCClient) GetPVC(context.Context, string, string) (*corev1.PersistentVolumeClaim, error) {
	return f.pvc.DeepCopy(), f.err
}

func (f *fakePVCClient) GetPV(context.Context, string) (*corev1.PersistentVolume, error) {
	return f.pv.DeepCopy(), f.err
}

func (f *fakePVCClient) ListPVs(context.Context) ([]corev1.PersistentVolume, error) {
	if f.pv == nil {
		return nil, f.err
	}
	return []corev1.PersistentVolume{*f.pv.DeepCopy()}, f.err
}

func (f *fakePVCClient) GetStorageClass(context.Context, string) (*storagev1.StorageClass, error) {
	return f.class.DeepCopy(), f.err
}

func (f *fakePVCClient) Warning(_ context.Context, _ *corev1.PersistentVolumeClaim, message string) error {
	f.warnings = append(f.warnings, message)
	return nil
}

func newPVCFixture(t *testing.T, params, annotations map[string]string) (*Driver, *btrfs.MockManager, *memStore, *fakePVCClient, *csi.CreateVolumeRequest) {
	t.Helper()
	d, mock, store := newTestDriverWithMock(t)
	className := "btrfs"
	client := &fakePVCClient{
		pvc: &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim", UID: "uid", Annotations: annotations},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &className},
		},
		class: &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: className},
			Provisioner: driverName, Parameters: maps.Clone(params),
		},
	}
	d.SetPVCClient(client)
	requestParams := maps.Clone(params)
	if requestParams == nil {
		requestParams = make(map[string]string)
	}
	requestParams["csi.storage.k8s.io/pvc/name"] = "claim"
	requestParams["csi.storage.k8s.io/pvc/namespace"] = "ns"
	return d, mock, store, client, settingsRequest("pvc-uid", requestParams)
}

func bindFixture(client *fakePVCClient, id string) {
	client.pvc.Spec.VolumeName = "pv"
	client.pvc.Status.Phase = corev1.ClaimBound
	client.pv = &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: "ns", Name: "claim", UID: client.pvc.UID},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: id},
			},
		},
	}
}

func TestPVCOverridesAndLiveReconciliation(t *testing.T) {
	d, mock, store, client, req := newPVCFixture(t,
		map[string]string{"cow": "false"}, map[string]string{cowAnnotation: "true", compressionAnnotation: "zstd"})
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Volume.VolumeId
	vol, _ := store.GetVolume(id)
	if vol.Nodatacow || vol.Options.Applied.Compression != "zstd" || vol.Options.PVC.UID != "uid" {
		t.Fatalf("PVC overrides not used: %+v", vol)
	}
	bindFixture(client, id)
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(mock.ApplyRootSettingsCalls) != 1 {
		t.Fatal("unchanged inputs caused a live mutation")
	}
	client.pvc.Annotations = nil
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	vol, _ = store.GetVolume(id)
	if !vol.Nodatacow || vol.Options.Applied.Compression != "none" {
		t.Fatalf("annotation removal did not use class default: %+v", vol.Options)
	}
	client.class.Parameters = map[string]string{"compression": "lzo"}
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	vol, _ = store.GetVolume(id)
	if vol.Nodatacow || vol.Options.Applied.Compression != "lzo" {
		t.Fatal("current class replacement was not applied")
	}
	client.err = errors.New("API unavailable")
	if _, err := d.CreateVolume(t.Context(), req); err != nil {
		t.Fatalf("idempotent creation should not need Kubernetes: %v", err)
	}
}

func TestPVCLiveFailureAndRecovery(t *testing.T) {
	d, mock, store, client, req := newPVCFixture(t, nil, nil)
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	bindFixture(client, resp.Volume.VolumeId)
	vol, _ := store.GetVolume(resp.Volume.VolumeId)
	statePath := filepath.Join(vol.BasePath, "state.json")
	fileStore, err := state.NewFileStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileStore.SaveVolume(vol); err != nil {
		t.Fatal(err)
	}
	d.store = fileStore
	client.pvc.Annotations = map[string]string{compressionAnnotation: "zstd"}
	mock.ApplyRootSettingsErr = errors.New("unsupported")
	if err := d.ReconcilePVCOptions(t.Context()); err == nil {
		t.Fatal("failed filesystem update should be reported")
	}
	vol, _ = fileStore.GetVolume(resp.Volume.VolumeId)
	if vol.Options.Pending == nil || vol.Options.Applied.Compression != "inherit" {
		t.Fatal("pending state was not persisted independently of applied state")
	}
	mock.ApplyRootSettingsErr = nil
	reopened, err := state.NewFileStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDriver(mock, reopened, "test-node")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Stop)
	restarted.SetPVCClient(client)
	if err := restarted.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	vol, _ = reopened.GetVolume(resp.Volume.VolumeId)
	if vol.Options.Pending != nil || vol.Options.Applied.Compression != "zstd" {
		t.Fatal("pending update did not recover")
	}
}

func TestPVCBindingValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakePVCClient)
	}{
		{"different volume", func(f *fakePVCClient) { f.pv.Spec.CSI.VolumeHandle = "other" }},
		{"different driver", func(f *fakePVCClient) { f.pv.Spec.CSI.Driver = "other" }},
		{"different claim UID", func(f *fakePVCClient) { f.pv.Spec.ClaimRef.UID = "other" }},
		{"different claim name", func(f *fakePVCClient) { f.pv.Spec.ClaimRef.Name = "other" }},
		{"different claim namespace", func(f *fakePVCClient) { f.pv.Spec.ClaimRef.Namespace = "other" }},
		{"missing claim reference", func(f *fakePVCClient) { f.pv.Spec.ClaimRef = nil }},
		{"missing CSI source", func(f *fakePVCClient) { f.pv.Spec.CSI = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, _, client, req := newPVCFixture(t, nil, nil)
			resp, err := d.CreateVolume(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			bindFixture(client, resp.Volume.VolumeId)
			client.pvc.Annotations = map[string]string{compressionAnnotation: "zstd"}
			tc.mutate(client)
			if err := d.ReconcilePVCOptions(t.Context()); err == nil {
				t.Fatal("unsafe binding accepted")
			}
			if len(mock.ApplyRootSettingsCalls) != 1 {
				t.Fatal("unsafe binding changed the filesystem")
			}
		})
	}
}

func TestPVCLiveInvalidInputAndUIDSafety(t *testing.T) {
	d, mock, store, client, req := newPVCFixture(t, nil, nil)
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	bindFixture(client, resp.Volume.VolumeId)
	client.pvc.Annotations = map[string]string{compressionAnnotation: "zstd:5"}
	for range 2 {
		if err := d.ReconcilePVCOptions(t.Context()); err == nil {
			t.Fatal("invalid live input should be reported")
		}
	}
	if len(client.warnings) != 1 || len(mock.ApplyRootSettingsCalls) != 1 {
		t.Fatal("invalid updates must preserve settings and rate-limit warnings")
	}
	client.pvc.Annotations[compressionAnnotation] = "lzo"
	client.pvc.UID = types.UID("replacement")
	client.pv.Spec.ClaimRef.UID = client.pvc.UID
	if err := d.ReconcilePVCOptions(t.Context()); err == nil {
		t.Fatal("replacement claim should not control retained data")
	}
	vol, _ := store.GetVolume(resp.Volume.VolumeId)
	if vol.Options.Applied.Compression != "inherit" {
		t.Fatal("replacement claim changed the volume")
	}
	client.err = errors.New("API unavailable")
	if err := d.ReconcilePVCOptions(t.Context()); err == nil {
		t.Fatal("API failure should be reported")
	}
	if len(mock.ApplyRootSettingsCalls) != 1 {
		t.Fatal("API failure changed the volume")
	}
}

func TestPVCLegacyAdoption(t *testing.T) {
	d, mock, store, client, _ := newPVCFixture(t, map[string]string{"compression": "lzo"}, nil)
	legacy := &state.Volume{ID: "legacy", Name: "old", BasePath: store.root(), Nodatacow: true}
	if err := store.SaveVolume(legacy); err != nil {
		t.Fatal(err)
	}
	mock.RootSettingsResult = btrfs.RootSettings{Nodatacow: true, Compression: "none"}
	bindFixture(client, legacy.ID)
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	vol, _ := store.GetVolume(legacy.ID)
	if vol.Options == nil || vol.Options.PVC.UID != "uid" || !vol.Nodatacow || len(mock.ApplyRootSettingsCalls) != 0 {
		t.Fatal("adoption must inspect and preserve existing settings")
	}
	client.pvc.Annotations = map[string]string{cowAnnotation: "true"}
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	vol, _ = store.GetVolume(legacy.ID)
	if vol.Nodatacow || vol.Options.Applied.Compression != "lzo" {
		t.Fatal("adopted volume did not accept live annotations")
	}
	if _, err := d.CreateVolume(t.Context(), settingsRequest("old", map[string]string{"cow": "false"})); err != nil {
		t.Fatalf("legacy creation retry after adoption and live edit: %v", err)
	}
}

func TestPVCCloneKeepsInheritedDefaultsUntilInputsChange(t *testing.T) {
	d, _, store, client, req := newPVCFixture(t, map[string]string{"compression": "zstd"}, nil)
	source := &state.Volume{ID: "source", Name: "source", BasePath: store.root(), Nodatacow: true,
		Options: &state.VolumeOptions{Applied: btrfs.RootSettings{Nodatacow: true, Compression: "none"}}}
	if err := store.SaveVolume(source); err != nil {
		t.Fatal(err)
	}
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: source.ID},
	}}
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	bindFixture(client, resp.Volume.VolumeId)
	// The source record has no matching PV and is deliberately not adopted.
	_ = d.ReconcilePVCOptions(t.Context())
	vol, _ := store.GetVolume(resp.Volume.VolumeId)
	if !vol.Nodatacow {
		t.Fatal("first reconciliation overwrote source inheritance")
	}
	client.pvc.Annotations = map[string]string{cowAnnotation: "true"}
	_ = d.ReconcilePVCOptions(t.Context())
	vol, _ = store.GetVolume(resp.Volume.VolumeId)
	if vol.Nodatacow || vol.Options.Applied.Compression != "zstd" {
		t.Fatal("post-bind edit did not apply target defaults")
	}
}

func TestRunPVCOptionsCancellation(t *testing.T) {
	d, _, _ := newTestDriverWithMock(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		d.RunPVCOptions(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciliation worker did not stop")
	}
}

type failingOptionStore struct {
	state.Store
	fail bool
}

func (s *failingOptionStore) SaveVolume(vol *state.Volume) error {
	// Match FileStore's behavior: its memory may advance even when disk persistence fails.
	if err := s.Store.SaveVolume(vol); err != nil {
		return err
	}
	if s.fail {
		return errors.New("state disk unavailable")
	}
	return nil
}

func TestPVCOptionsPersistPendingBeforeEveryAttempt(t *testing.T) {
	d, mock, _, client, req := newPVCFixture(t, nil, nil)
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	bindFixture(client, resp.Volume.VolumeId)
	client.pvc.Annotations = map[string]string{compressionAnnotation: "zstd"}
	store := &failingOptionStore{Store: d.store, fail: true}
	d.store = store
	for range 2 {
		if err := d.ReconcilePVCOptions(t.Context()); err == nil {
			t.Fatal("failed state persistence must fail every attempt")
		}
		if len(mock.ApplyRootSettingsCalls) != 1 {
			t.Fatal("filesystem changed without a successful pending-state save")
		}
	}
	store.fail = false
	if err := d.ReconcilePVCOptions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(mock.ApplyRootSettingsCalls) != 2 {
		t.Fatal("persisted pending operation was not applied")
	}
}
