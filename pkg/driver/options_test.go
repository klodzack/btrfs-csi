package driver

import (
	"testing"

	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveRootSettings(t *testing.T) {
	for _, tc := range []struct {
		name        string
		params, ann map[string]string
		want        btrfs.RootSettings
		invalid     bool
	}{
		{"defaults", nil, nil, btrfs.RootSettings{Compression: "inherit"}, false},
		{"class compression", map[string]string{"compression": "ZSTD"}, nil, btrfs.RootSettings{Compression: "zstd"}, false},
		{"override cow", map[string]string{"cow": "false"}, map[string]string{cowAnnotation: "TRUE"}, btrfs.RootSettings{Compression: "inherit"}, false},
		{"disable inherited compression", map[string]string{"compression": "zstd"}, map[string]string{cowAnnotation: "false"}, btrfs.RootSettings{Nodatacow: true, Compression: "none"}, false},
		{"class cow suppresses compression", map[string]string{"cow": "false", "compression": "zstd"}, nil, btrfs.RootSettings{Nodatacow: true, Compression: "none"}, false},
		{"compression override", map[string]string{"compression": "zlib"}, map[string]string{compressionAnnotation: "lzo"}, btrfs.RootSettings{Compression: "lzo"}, false},
		{"none override", map[string]string{"compression": "zstd"}, map[string]string{compressionAnnotation: "none"}, btrfs.RootSettings{Compression: "none"}, false},
		{"inherit override", map[string]string{"compression": "zstd"}, map[string]string{compressionAnnotation: "inherit"}, btrfs.RootSettings{Compression: "inherit"}, false},
		{"explicit conflict", nil, map[string]string{cowAnnotation: "false", compressionAnnotation: "zstd"}, btrfs.RootSettings{}, true},
		{"compression cannot enable cow", map[string]string{"cow": "false"}, map[string]string{compressionAnnotation: "zstd"}, btrfs.RootSettings{}, true},
		{"bad cow", nil, map[string]string{cowAnnotation: "yes"}, btrfs.RootSettings{}, true},
		{"empty compression", nil, map[string]string{compressionAnnotation: ""}, btrfs.RootSettings{}, true},
		{"compression level", map[string]string{"compression": "zstd:5"}, nil, btrfs.RootSettings{}, true},
		{"separate level", map[string]string{"compression-level": "5"}, nil, btrfs.RootSettings{}, true},
		{"annotation level", nil, map[string]string{"btrfs.csi.local/compression-level": "5"}, btrfs.RootSettings{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRootSettings(tc.params, tc.ann)
			if tc.invalid {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("expected InvalidArgument, got %v", err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("resolve = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestSettingsFingerprint(t *testing.T) {
	first := settingsFingerprint(map[string]string{"compression": "zstd", "pool": "a"}, nil)
	if first != settingsFingerprint(map[string]string{"compression": "zstd", "pool": "b"}, map[string]string{"unrelated": "value"}) {
		t.Fatal("unrelated settings must not trigger root updates")
	}
	if first == settingsFingerprint(map[string]string{"compression": "zstd"}, map[string]string{compressionAnnotation: "none"}) {
		t.Fatal("PVC option changes must trigger updates")
	}
	if settingsFingerprint(nil, nil) == settingsFingerprint(nil, map[string]string{cowAnnotation: ""}) {
		t.Fatal("empty and absent values must differ")
	}
}
