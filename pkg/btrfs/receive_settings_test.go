package btrfs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

var errReceiveSettings = errors.New("received root failure")

type receivedRootHarness struct {
	source, target *kernelRootFlags
	readonly       bool
	opened         bool
	sealed         bool
	fail           string
}

func (h *receivedRootHarness) run(ctx context.Context, name string, args ...string) (string, error) {
	if slices.Equal(args, []string{"property", "set", "-f", "/received", "ro", "false"}) {
		if h.fail == "open" {
			return "", errReceiveSettings
		}
		h.readonly, h.opened = false, true
		return "", nil
	}
	if slices.Equal(args, []string{"property", "set", "/received", "ro", "true"}) {
		if h.fail == "seal" {
			return "", errReceiveSettings
		}
		h.readonly, h.sealed = true, true
		return "", nil
	}
	source := slices.Contains(args, "/frozen-source")
	if source {
		if h.fail == "source" {
			return "", errReceiveSettings
		}
		return h.source.run(ctx, name, args...)
	}
	mutating := name == "chattr" || (name == "btrfs" && args[1] == "set")
	if mutating && h.readonly {
		return "", errors.New("cannot modify readonly received root")
	}
	if mutating && h.fail == "apply" {
		return "", errReceiveSettings
	}
	return h.target.run(ctx, name, args...)
}

func flagsForSettings(settings RootSettings) *kernelRootFlags {
	flags := &kernelRootFlags{nocow: settings.Nodatacow, compression: settings.Compression}
	if flags.nocow {
		flags.compression = "inherit"
	}
	return flags
}

func TestFinalizeReceivedRootSettings(t *testing.T) {
	for _, source := range []RootSettings{
		{Compression: "inherit"},
		{Nodatacow: true, Compression: "none"},
		{Compression: "zstd"},
	} {
		for _, readonly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%+v/readonly=%v", source, readonly), func(t *testing.T) {
				saved := runCommand
				t.Cleanup(func() { runCommand = saved })
				h := &receivedRootHarness{
					source: flagsForSettings(source), target: &kernelRootFlags{compression: "zstd"}, readonly: true,
				}
				runCommand = h.run
				m := &RealManager{}
				if err := m.finalizeReceivedRoot(t.Context(), "/frozen-source", "/received", readonly); err != nil {
					t.Fatal(err)
				}
				actual, err := m.GetRootSettings(t.Context(), "/received")
				if err != nil || actual != source || h.readonly != readonly {
					t.Fatalf("received = %+v, %v; readonly=%v; want %+v, %v", actual, err, h.readonly, source, readonly)
				}
				if readonly && source.Compression == "zstd" && h.opened {
					t.Fatal("unchanged readonly receive should retain its received UUID")
				}
			})
		}
	}
}

func TestFinalizeReceivedRootFailures(t *testing.T) {
	for _, stage := range []string{"source", "open", "apply", "seal"} {
		t.Run(stage, func(t *testing.T) {
			saved := runCommand
			t.Cleanup(func() { runCommand = saved })
			h := &receivedRootHarness{
				source: &kernelRootFlags{compression: "inherit"},
				target: &kernelRootFlags{compression: "zstd"}, readonly: true, fail: stage,
			}
			runCommand = h.run
			err := (&RealManager{}).finalizeReceivedRoot(t.Context(), "/frozen-source", "/received", true)
			if !errors.Is(err, errReceiveSettings) {
				t.Fatalf("failure was not propagated: %v", err)
			}
			if h.sealed {
				t.Fatal("failed normalization must not publish a sealed snapshot")
			}
		})
	}
}
