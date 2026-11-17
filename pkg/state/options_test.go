package state

import (
	"encoding/json"
	"testing"

	"github.com/mporrato/btrfs-csi/pkg/btrfs"
)

func TestVolumeOptionsPersistenceAndIsolation(t *testing.T) {
	store, err := NewFileStore(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	settings := btrfs.RootSettings{Compression: "zstd"}
	pending := settings
	vol := &Volume{ID: "vol", Options: &VolumeOptions{
		PVC:     PVCReference{Namespace: "ns", Name: "claim", UID: "uid"},
		Request: settings, Creation: settings, Applied: settings,
		Pending: &pending, InputFingerprint: "inputs",
	}}
	if err := store.SaveVolume(vol); err != nil {
		t.Fatal(err)
	}
	vol.Options.Pending.Compression = "none"
	got, _ := store.GetVolume("vol")
	if got.Options.Pending.Compression != "zstd" {
		t.Fatal("SaveVolume must copy nested option state")
	}
	got.Options.Pending.Compression = "lzo"
	again, _ := store.GetVolume("vol")
	if again.Options.Pending.Compression != "zstd" {
		t.Fatal("GetVolume must copy nested option state")
	}
	reopened, err := NewFileStore(store.Dir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reopened.GetVolume("vol")
	if got.Options.PVC.UID != "uid" || got.Options.Applied != settings || got.Options.InputFingerprint != "inputs" {
		t.Fatalf("options did not survive restart: %+v", got.Options)
	}
	var legacy Volume
	if err := json.Unmarshal([]byte(`{"ID":"old","Nodatacow":true}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Options != nil || !legacy.Nodatacow {
		t.Fatal("legacy JSON must remain unchanged")
	}
}

func TestSnapshotRootSettingsIsolation(t *testing.T) {
	store, err := NewFileStore(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{ID: "snap", RootSettings: &btrfs.RootSettings{Compression: "zstd"}}
	if err := store.SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	snap.RootSettings.Compression = "none"
	got, _ := store.GetSnapshot("snap")
	got.RootSettings.Compression = "lzo"
	got, _ = store.GetSnapshot("snap")
	if got.RootSettings.Compression != "zstd" {
		t.Fatal("snapshot options must be deeply copied")
	}
}
