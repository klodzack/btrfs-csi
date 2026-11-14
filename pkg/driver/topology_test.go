package driver

import (
	"context"
	"os"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mporrato/btrfs-csi/pkg/btrfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateVolume_Topology(t *testing.T) {
	local := &csi.Topology{Segments: map[string]string{topologyKey: "test-node"}}
	remote := &csi.Topology{Segments: map[string]string{topologyKey: "other-node"}}
	for _, tc := range []struct {
		name string
		req  *csi.TopologyRequirement
		code codes.Code
	}{
		{name: "unconstrained"},
		{name: "empty", req: &csi.TopologyRequirement{}},
		{name: "local", req: &csi.TopologyRequirement{Requisite: []*csi.Topology{local}}},
		{name: "one matching segment", req: &csi.TopologyRequirement{Requisite: []*csi.Topology{remote, local}}},
		{name: "preference is advisory", req: &csi.TopologyRequirement{
			Requisite: []*csi.Topology{local}, Preferred: []*csi.Topology{remote},
		}},
		{name: "preferred only", req: &csi.TopologyRequirement{Preferred: []*csi.Topology{remote}}},
		{name: "remote", req: &csi.TopologyRequirement{Requisite: []*csi.Topology{remote}}, code: codes.ResourceExhausted},
		{name: "unsupported key", req: &csi.TopologyRequirement{Requisite: []*csi.Topology{
			{Segments: map[string]string{topologyKey: "test-node", "zone": "unknown"}},
		}}, code: codes.ResourceExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, store := newTestDriverWithMock(t)
			req := &csi.CreateVolumeRequest{
				Name: "topology-volume", VolumeCapabilities: singleNodeWriterCap(),
				AccessibilityRequirements: tc.req,
			}
			resp, err := d.CreateVolume(context.Background(), req)
			if status.Code(err) != tc.code {
				t.Fatalf("CreateVolume = %v, want %v", err, tc.code)
			}
			if err != nil {
				if len(mock.CreateSubvolumeCalls) != 0 || len(store.ListVolumes()) != 0 {
					t.Fatal("incompatible topology modified storage")
				}
				entries, readErr := os.ReadDir(store.root())
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("incompatible topology modified filesystem: %v, %v", entries, readErr)
				}
				return
			}
			if got := resp.Volume.AccessibleTopology; len(got) != 1 || got[0].Segments[topologyKey] != "test-node" {
				t.Fatalf("unexpected accessible topology: %v", got)
			}
			retry, err := d.CreateVolume(context.Background(), req)
			if err != nil || retry.Volume.VolumeId != resp.Volume.VolumeId || len(mock.CreateSubvolumeCalls) != 1 {
				t.Fatalf("retry was not idempotent: %v, %v", retry, err)
			}
			req.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{remote}}
			if _, err := d.CreateVolume(context.Background(), req); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("incompatible retry = %v, want ResourceExhausted", err)
			}
		})
	}
}

func TestGetCapacity_Topology(t *testing.T) {
	for _, tc := range []struct {
		name     string
		topology *csi.Topology
		want     int64
	}{
		{name: "unconstrained", want: 1024},
		{name: "local", topology: &csi.Topology{Segments: map[string]string{topologyKey: "test-node"}}, want: 1024},
		{name: "remote", topology: &csi.Topology{Segments: map[string]string{topologyKey: "other-node"}}},
		{name: "unknown key", topology: &csi.Topology{Segments: map[string]string{"zone": "unknown"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, _ := newTestDriverWithMock(t)
			mock.GetFilesystemUsageResult = &btrfs.FsUsage{Available: 1024}
			resp, err := d.GetCapacity(context.Background(), &csi.GetCapacityRequest{AccessibleTopology: tc.topology})
			if err != nil || resp.AvailableCapacity != tc.want {
				t.Fatalf("GetCapacity = %v, %v; want %d", resp, err, tc.want)
			}
			if tc.want == 0 && len(mock.GetFilesystemUsageCalls) != 0 {
				t.Fatal("queried local capacity for inaccessible topology")
			}
		})
	}
}
