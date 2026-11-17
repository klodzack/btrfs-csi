package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"github.com/mporrato/btrfs-csi/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (d *Driver) volumeRootSettings(ctx context.Context, vol *state.Volume) (btrfs.RootSettings, error) {
	if vol.Options != nil && vol.Options.Pending == nil {
		return vol.Options.Applied, nil
	}
	return d.manager.GetRootSettings(ctx, vol.Path())
}

func (d *Driver) sourceRootSettings(ctx context.Context, src *csi.VolumeContentSource,
	fallback btrfs.RootSettings) (btrfs.RootSettings, error) {
	snapID, volID := contentSourceIDs(src)
	if volID != "" {
		vol, ok := d.store.GetVolume(volID)
		if !ok {
			return btrfs.RootSettings{}, status.Errorf(codes.NotFound, "source volume %s not found", volID)
		}
		settings, err := d.volumeRootSettings(ctx, vol)
		if err != nil {
			return btrfs.RootSettings{}, status.Errorf(codes.Internal, "read source settings: %v", err)
		}
		return settings, nil
	}
	if snapID != "" {
		snap, ok := d.store.GetSnapshot(snapID)
		if !ok {
			return btrfs.RootSettings{}, status.Errorf(codes.NotFound, "snapshot %s not found", snapID)
		}
		if snap.RootSettings != nil {
			return *snap.RootSettings, nil
		}
		settings, err := d.manager.GetRootSettings(ctx, snap.Path())
		if err != nil {
			return btrfs.RootSettings{}, status.Errorf(codes.Internal, "read snapshot settings: %v", err)
		}
		return settings, nil
	}
	return fallback, nil
}

const (
	cowAnnotation         = driverName + "/cow"
	compressionAnnotation = driverName + "/compression"
)

func resolveRootSettings(params, annotations map[string]string) (btrfs.RootSettings, error) {
	for _, key := range []string{"compression-level", "compressionLevel"} {
		_, parameter := params[key]
		_, annotation := annotations[driverName+"/"+key]
		if parameter || annotation {
			return btrfs.RootSettings{}, status.Error(codes.InvalidArgument,
				"compression levels cannot be configured per subvolume")
		}
	}
	values := maps.Clone(params)
	if values == nil {
		values = make(map[string]string)
	}
	for _, key := range []string{"cow", "compression"} {
		if value, ok := annotations[driverName+"/"+key]; ok {
			values[key] = value
		}
	}
	nodatacow, err := parseCowParameter(values)
	if err != nil {
		return btrfs.RootSettings{}, err
	}
	compression := "inherit"
	if value, ok := values["compression"]; ok {
		compression = strings.ToLower(value)
	}
	settings := btrfs.RootSettings{Compression: compression}
	if err := settings.Validate(); err != nil {
		return btrfs.RootSettings{}, status.Error(codes.InvalidArgument, err.Error())
	}
	if nodatacow {
		explicit, ok := annotations[compressionAnnotation]
		if ok && explicit != "" && compression != "inherit" && compression != "none" {
			return btrfs.RootSettings{}, status.Error(codes.InvalidArgument, "PVC compression requires cow=true")
		}
		settings.Nodatacow = true
		settings.Compression = "none"
	}
	return settings, nil
}

func settingsFingerprint(params, annotations map[string]string) string {
	values := make(map[string]string)
	for _, key := range []string{"cow", "compression", "compression-level", "compressionLevel"} {
		if value, ok := params[key]; ok {
			values["class/"+key] = value
		}
		if value, ok := annotations[driverName+"/"+key]; ok {
			values["pvc/"+key] = value
		}
	}
	data, _ := json.Marshal(values) //nolint:errchkjson // A map containing only strings cannot fail JSON encoding.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
