package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failingVolumeStore struct {
	state.Store
}

func (s failingVolumeStore) SaveVolume(*state.Volume) error {
	return errors.New("save failed")
}

func TestNodeExpandVolume_QuotaAndState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		current    int64
		requested  int64
		limit      int64
		quotaErr   bool
		saveErr    bool
		missing    bool
		code       codes.Code
		wantCalls  int
		wantStored int64
	}{
		{name: "expand", current: 1024, requested: 2048, wantCalls: 1, wantStored: 2048},
		{name: "enable quota for unlimited volume", requested: 2048, wantCalls: 1, wantStored: 2048},
		{name: "same capacity", current: 1024, requested: 1024, wantStored: 1024},
		{name: "no requested capacity", current: 1024, wantStored: 1024},
		{name: "shrink", current: 2048, requested: 1024, code: codes.InvalidArgument, wantStored: 2048},
		{name: "exceeds limit", current: 1024, requested: 4096, limit: 2048, code: codes.InvalidArgument, wantStored: 1024},
		{name: "missing volume", requested: 2048, missing: true, code: codes.NotFound},
		{name: "quota failure", current: 1024, requested: 2048, quotaErr: true, code: codes.Internal, wantCalls: 1, wantStored: 1024},
		{name: "save failure", current: 1024, requested: 2048, saveErr: true, code: codes.Internal, wantCalls: 1, wantStored: 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, store := newTestDriverWithMock(t)
			vol := &state.Volume{ID: "expand", Name: "expand", CapacityBytes: tc.current}
			if !tc.missing {
				if err := store.SaveVolume(vol); err != nil {
					t.Fatal(err)
				}
			}
			if tc.quotaErr {
				mock.SetQgroupLimitErr = errors.New("quota failed")
			}
			if tc.saveErr {
				d.store = failingVolumeStore{Store: d.store}
			}
			req := &csi.NodeExpandVolumeRequest{
				VolumeId: vol.ID, VolumePath: t.TempDir(),
				CapacityRange: &csi.CapacityRange{RequiredBytes: tc.requested, LimitBytes: tc.limit},
			}
			resp, err := d.NodeExpandVolume(context.Background(), req)
			if status.Code(err) != tc.code {
				t.Fatalf("NodeExpandVolume = %v, want %v", err, tc.code)
			}
			if !tc.missing {
				stored, _ := store.GetVolume(vol.ID)
				if stored.CapacityBytes != tc.wantStored {
					t.Fatalf("stored capacity = %d, want %d", stored.CapacityBytes, tc.wantStored)
				}
			}
			if err == nil {
				if resp.CapacityBytes != tc.wantStored {
					t.Fatalf("response capacity = %d, want %d", resp.CapacityBytes, tc.wantStored)
				}
				if _, err := d.NodeExpandVolume(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			if len(mock.SetQgroupLimitCalls) != tc.wantCalls {
				t.Fatalf("quota calls = %d, want %d", len(mock.SetQgroupLimitCalls), tc.wantCalls)
			}
			if tc.wantCalls > 0 && mock.SetQgroupLimitCalls[0].Bytes != uint64(tc.requested) {
				t.Fatalf("quota limit = %d, want %d", mock.SetQgroupLimitCalls[0].Bytes, tc.requested)
			}
		})
	}
}

func TestGetPluginCapabilities_OnlineExpansion(t *testing.T) {
	d := newTestDriver(t)
	resp, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range resp.Capabilities {
		if cap.GetVolumeExpansion().GetType() == csi.PluginCapability_VolumeExpansion_ONLINE {
			return
		}
	}
	t.Fatal("missing online expansion capability")
}
