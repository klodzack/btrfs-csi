//go:build integration

package btrfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootSettingsInheritanceAndIsolation(t *testing.T) {
	mnt := setupLoopbackBtrfs(t)
	m := &RealManager{}
	volume := filepath.Join(mnt, "volume")
	neighbor := filepath.Join(mnt, "neighbor")
	for _, path := range []string{volume, neighbor} {
		if err := m.CreateSubvolume(t.Context(), path, CreateSubvolumeOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	compressed := RootSettings{Compression: "zstd"}
	if err := m.ApplyRootSettings(t.Context(), volume, compressed); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(volume, "old-dir")
	if err := os.Mkdir(oldDir, 0o750); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(volume, "old-file")
	if err := os.WriteFile(oldFile, []byte("keep existing file settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(mnt, "snapshot")
	if err := m.CreateSnapshot(t.Context(), volume, snapshot, true); err != nil {
		t.Fatal(err)
	}
	nocow := RootSettings{Nodatacow: true, Compression: "none"}
	if err := m.ApplyRootSettings(t.Context(), volume, nocow); err != nil {
		t.Fatal(err)
	}
	nocowFile := filepath.Join(volume, "new-nocow")
	if err := os.WriteFile(nocowFile, []byte("new data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want RootSettings
	}{
		{volume, nocow}, {nocowFile, nocow}, {oldDir, compressed}, {oldFile, compressed},
		{snapshot, compressed}, {neighbor, RootSettings{Compression: "inherit"}},
	} {
		assertRootSettings(t, m, tc.path, tc.want)
	}
	if data, err := os.ReadFile(oldFile); err != nil || string(data) != "keep existing file settings" {
		t.Fatalf("root update changed existing data: %q, %v", data, err)
	}
	if err := m.ApplyRootSettings(t.Context(), volume, RootSettings{Compression: "lzo"}); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(volume, "new-compressed")
	if err := os.WriteFile(newFile, []byte("compressed data"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRootSettings(t, m, newFile, RootSettings{Compression: "lzo"})
	assertRootSettings(t, m, nocowFile, nocow)
	if err := m.ApplyRootSettings(t.Context(), volume, RootSettings{Compression: "inherit"}); err != nil {
		t.Fatal(err)
	}
	assertRootSettings(t, m, volume, RootSettings{Compression: "inherit"})
}

func assertRootSettings(t *testing.T, m *RealManager, path string, want RootSettings) {
	t.Helper()
	got, err := m.GetRootSettings(t.Context(), path)
	if err != nil || got != want {
		t.Fatalf("%s settings = %+v, %v; want %+v", path, got, err, want)
	}
}

func TestNocowSubvolumeUnderCompressedParent(t *testing.T) {
	mnt := setupLoopbackBtrfs(t)
	m := &RealManager{}
	if err := m.ApplyRootSettings(t.Context(), mnt, RootSettings{Compression: "zstd"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mnt, "nocow")
	if err := m.CreateSubvolume(t.Context(), path, CreateSubvolumeOptions{Nodatacow: true}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetRootSettings(t.Context(), path)
	if err != nil || got != (RootSettings{Nodatacow: true, Compression: "none"}) {
		t.Fatalf("NOCOW subvolume settings = %+v, %v", got, err)
	}
}
