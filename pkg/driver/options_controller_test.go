package driver

import (
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
