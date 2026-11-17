package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"github.com/mporrato/btrfs-csi/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type receivedRootManager struct {
	*btrfs.MockManager
}

func (m *receivedRootManager) CreateSnapshot(ctx context.Context, src, dst string, readonly bool) error {
	if err := m.MockManager.CreateSnapshot(ctx, src, dst, readonly); err != nil {
		return err
	}
	// Model a receive that retained the destination parent's compression and lost NOCOW.
	m.RootSettingsByPath[dst] = btrfs.RootSettings{Compression: "zstd"}
	return nil
}

func settingsRequest(name string, params map[string]string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{Name: name, VolumeCapabilities: singleNodeWriterCap(), Parameters: params}
}

func TestCreateVolumeCompressionAndLiveRetry(t *testing.T) {
	d, mock, store := newTestDriverWithMock(t)
	req := settingsRequest("compressed", map[string]string{"compression": "zstd"})
	resp, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.ApplyRootSettingsCalls) != 1 || mock.ApplyRootSettingsCalls[0].Settings.Compression != "zstd" {
		t.Fatalf("compression was not applied: %+v", mock.ApplyRootSettingsCalls)
	}
	vol, _ := store.GetVolume(resp.Volume.VolumeId)
	if vol.Options == nil || vol.Options.Creation.Compression != "zstd" {
		t.Fatalf("missing creation settings: %+v", vol)
	}
	vol.Nodatacow = true
	vol.Options.Applied = btrfs.RootSettings{Nodatacow: true, Compression: "none"}
	if err := store.SaveVolume(vol); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateVolume(t.Context(), req); err != nil {
		t.Fatalf("retry after live changes: %v", err)
	}
	if len(mock.ApplyRootSettingsCalls) != 1 {
		t.Fatal("creation retry reapplied settings")
	}
	req.Parameters["compression"] = "lzo"
	if _, err := d.CreateVolume(t.Context(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("incompatible compression retry = %v", err)
	}
}

func TestCreateVolumeSettingsFailureCleanup(t *testing.T) {
	d, mock, store := newTestDriverWithMock(t)
	mock.ApplyRootSettingsErr = errors.New("unsupported")
	if _, err := d.CreateVolume(t.Context(), settingsRequest("fail", map[string]string{"compression": "zstd"})); err == nil {
		t.Fatal("settings failure should fail provisioning")
	}
	if len(mock.DeleteSubvolumeCalls) != 1 || len(store.ListVolumes()) != 0 {
		t.Fatal("failed settings application left an orphan")
	}
}

func TestSnapshotSettingsSurviveSourceDeletion(t *testing.T) {
	d, _, store := newTestDriverWithMock(t)
	src, err := d.CreateVolume(t.Context(), settingsRequest("source", map[string]string{"cow": "false"}))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := d.CreateSnapshot(t.Context(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: src.Volume.VolumeId})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := store.GetSnapshot(snap.Snapshot.SnapshotId)
	if record.RootSettings == nil || !record.RootSettings.Nodatacow {
		t.Fatal("snapshot did not preserve root settings")
	}
	if err := store.DeleteVolume(src.Volume.VolumeId); err != nil {
		t.Fatal(err)
	}
	req := settingsRequest("restore", map[string]string{"compression": "zstd"})
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snap.Snapshot.SnapshotId},
	}}
	restored, err := d.CreateVolume(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	vol, _ := store.GetVolume(restored.Volume.VolumeId)
	if !vol.Nodatacow || vol.Options.Applied.Compression != "none" {
		t.Fatalf("restore did not inherit snapshot defaults: %+v", vol.Options)
	}
	if _, err := d.CreateVolume(t.Context(), req); err != nil {
		t.Fatalf("restore retry after source deletion: %v", err)
	}
}

func TestCreateVolumeRejectsCompressionLevel(t *testing.T) {
	d, mock, _ := newTestDriverWithMock(t)
	if _, err := d.CreateVolume(t.Context(), settingsRequest("level", map[string]string{"compression": "zstd:5"})); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if len(mock.CreateSubvolumeCalls) != 0 {
		t.Fatal("invalid settings created a subvolume")
	}
}

func TestCrossPoolVolumeRootSettingsMatchRecordedState(t *testing.T) {
	for _, mode := range []string{"clone", "restore"} {
		for _, cow := range []string{"true", "false"} {
			t.Run(mode+"/cow="+cow, func(t *testing.T) {
				d, mock, store := newTestDriverWithMock(t)
				target := t.TempDir()
				d.store.(*state.MultiStore).AddStoreForTest(target, newMemStore(target))
				d.SetPools(map[string]string{"default": store.root(), "target": target})
				source, err := d.CreateVolume(t.Context(), settingsRequest("source", map[string]string{"cow": cow}))
				if err != nil {
					t.Fatal(err)
				}
				original, _ := store.GetVolume(source.Volume.VolumeId)
				content := &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
					Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: original.ID},
				}}
				if mode == "restore" {
					snap, err := d.CreateSnapshot(t.Context(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: original.ID})
					if err != nil {
						t.Fatal(err)
					}
					content.Type = &csi.VolumeContentSource_Snapshot{
						Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snap.Snapshot.SnapshotId},
					}
				}
				d.manager = &receivedRootManager{mock}
				req := settingsRequest("received", map[string]string{"pool": "target"})
				req.VolumeContentSource = content
				resp, err := d.CreateVolume(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				vol, _ := d.store.GetVolume(resp.Volume.VolumeId)
				actual, err := d.manager.GetRootSettings(t.Context(), vol.Path())
				if err != nil || actual != original.Options.Applied || vol.Options.Applied != actual || vol.Options.Creation != actual {
					t.Fatalf("received root = %+v, %v; state = %+v; source = %+v", actual, err, vol.Options, original.Options.Applied)
				}
			})
		}
	}
}

func TestCloneRootSettingsFailureCleansUp(t *testing.T) {
	d, mock, store := newTestDriverWithMock(t)
	source, err := d.CreateVolume(t.Context(), settingsRequest("source", nil))
	if err != nil {
		t.Fatal(err)
	}
	mock.ApplyRootSettingsErr = errors.New("cannot apply received root settings")
	req := settingsRequest("clone", nil)
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: source.Volume.VolumeId},
	}}
	if _, err := d.CreateVolume(t.Context(), req); err == nil {
		t.Fatal("failed root settings must prevent publishing the clone")
	}
	if len(store.ListVolumes()) != 1 || len(mock.DeleteSubvolumeCalls) != 1 {
		t.Fatal("failed clone settings left state or a subvolume behind")
	}
}

func TestSnapshotRecordsActualRootSettings(t *testing.T) {
	d, mock, store := newTestDriverWithMock(t)
	source, err := d.CreateVolume(t.Context(), settingsRequest("source", nil))
	if err != nil {
		t.Fatal(err)
	}
	d.manager = &receivedRootManager{mock}
	resp, err := d.CreateSnapshot(t.Context(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: source.Volume.VolumeId})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := store.GetSnapshot(resp.Snapshot.SnapshotId)
	actual, err := d.manager.GetRootSettings(t.Context(), snap.Path())
	if err != nil || snap.RootSettings == nil || *snap.RootSettings != actual {
		t.Fatalf("snapshot metadata = %+v; actual = %+v, %v", snap.RootSettings, actual, err)
	}
}

func TestSnapshotRootInspectionFailureCleansUp(t *testing.T) {
	d, mock, store := newTestDriverWithMock(t)
	source, err := d.CreateVolume(t.Context(), settingsRequest("source", nil))
	if err != nil {
		t.Fatal(err)
	}
	mock.GetRootSettingsErr = errors.New("cannot inspect snapshot root")
	_, err = d.CreateSnapshot(t.Context(), &csi.CreateSnapshotRequest{Name: "snap", SourceVolumeId: source.Volume.VolumeId})
	if status.Code(err) != codes.Internal {
		t.Fatalf("inspection failure must prevent saving the snapshot: %v", err)
	}
	if len(store.ListSnapshots()) != 0 || len(mock.DeleteSubvolumeCalls) != 1 {
		t.Fatal("failed inspection left snapshot state or a subvolume behind")
	}
}
