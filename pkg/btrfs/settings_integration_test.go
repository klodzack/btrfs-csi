//go:build integration

package btrfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
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

func TestNocowTransitionsFromDisabledCompression(t *testing.T) {
	mnt := setupLoopbackBtrfs(t)
	m := &RealManager{}
	path := filepath.Join(mnt, "volume")
	if err := m.CreateSubvolume(t.Context(), path, CreateSubvolumeOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, settings := range []RootSettings{
		{Compression: "none"},
		{Nodatacow: true, Compression: "none"},
		{Compression: "none"},
	} {
		if err := m.ApplyRootSettings(t.Context(), path, settings); err != nil {
			t.Fatal(err)
		}
		assertRootSettings(t, m, path, settings)
	}
}

func TestCrossFilesystemSnapshotPreservesRootSettings(t *testing.T) {
	sourcePool := setupLoopbackBtrfs(t)
	targetPool := setupLoopbackBtrfs(t)
	for _, tc := range []struct {
		name   string
		source RootSettings
		target RootSettings
	}{
		{"inherit-to-zstd", RootSettings{Compression: "inherit"}, RootSettings{Compression: "zstd"}},
		{"nocow-to-zstd", RootSettings{Nodatacow: true, Compression: "none"}, RootSettings{Compression: "zstd"}},
		{"none-to-zstd", RootSettings{Compression: "none"}, RootSettings{Compression: "zstd"}},
		{"zstd-to-lzo", RootSettings{Compression: "zstd"}, RootSettings{Compression: "lzo"}},
	} {
		for _, readonly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readonly=%v", tc.name, readonly), func(t *testing.T) {
				checkReceivedRootSettings(t, sourcePool, targetPool, tc.source, tc.target, readonly)
			})
		}
	}
}

func checkReceivedRootSettings(t *testing.T, sourcePool, targetPool string,
	sourceSettings, targetSettings RootSettings, readonly bool) {
	t.Helper()
	m := &RealManager{}
	if err := m.ApplyRootSettings(t.Context(), targetPool, targetSettings); err != nil {
		t.Fatal(err)
	}
	name := uuid.NewString()
	source := filepath.Join(sourcePool, name)
	target := filepath.Join(targetPool, name)
	if err := m.CreateSubvolume(t.Context(), source, CreateSubvolumeOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyRootSettings(t.Context(), source, sourceSettings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("source data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSnapshot(t.Context(), source, target, readonly); err != nil {
		t.Fatal(err)
	}
	assertRootSettings(t, m, target, sourceSettings)
	assertRootSettings(t, m, source, sourceSettings)
	assertRootSettings(t, m, targetPool, targetSettings)
	data, err := os.ReadFile(filepath.Join(target, "data"))
	if err != nil || string(data) != "source data" {
		t.Fatalf("received data = %q, %v", data, err)
	}
	err = os.WriteFile(filepath.Join(target, "new-data"), []byte("new data"), 0o600)
	if readonly && err == nil {
		t.Fatal("received snapshot is not readonly")
	}
	if !readonly && err != nil {
		t.Fatalf("received clone is not writable: %v", err)
	}
}
