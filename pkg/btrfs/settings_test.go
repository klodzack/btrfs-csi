package btrfs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGetRootSettings(t *testing.T) {
	for _, tc := range []struct {
		name, attrs, property string
		want                  RootSettings
	}{
		{"inherited", "---------------------- /pool/vol", "compression=\n", RootSettings{Compression: "inherit"}},
		{"algorithm", "--------c------------- /pool/vol", "compression=zstd\n", RootSettings{Compression: "zstd"}},
		{"disabled", "---------------------m /pool/vol", "compression=\n", RootSettings{Compression: "none"}},
		{"nocow", "---------------C------ /pool/vol", "compression=\n", RootSettings{Nodatacow: true, Compression: "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := runCommand
			t.Cleanup(func() { runCommand = saved })
			runCommand = func(_ context.Context, name string, args ...string) (string, error) {
				if name == "lsattr" && reflect.DeepEqual(args, []string{"-d", "/pool/vol"}) {
					return tc.attrs, nil
				}
				if name == "btrfs" && reflect.DeepEqual(args, []string{"property", "get", "/pool/vol", "compression"}) {
					return tc.property, nil
				}
				t.Fatalf("unexpected command %s %q", name, args)
				return "", nil
			}
			got, err := (&RealManager{}).GetRootSettings(t.Context(), "/pool/vol")
			if err != nil || got != tc.want {
				t.Fatalf("GetRootSettings = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestApplyRootSettings(t *testing.T) {
	for _, tc := range []struct {
		name, attrs, property string
		settings              RootSettings
		want                  [][]string
	}{
		{"disable cow", "--------c-------------", "compression=zstd", RootSettings{true, "none"}, [][]string{
			{"btrfs", "property", "set", "/pool/vol", "compression", "none"},
			{"chattr", "+C", "/pool/vol"},
		}},
		{"enable compression", "---------------C------", "compression=", RootSettings{false, "zstd"}, [][]string{
			{"chattr", "-C", "/pool/vol"},
			{"btrfs", "property", "set", "/pool/vol", "compression", "zstd"},
		}},
		{"enable cow with compression disabled", "---------------C------", "compression=", RootSettings{false, "none"}, [][]string{
			{"chattr", "-C", "/pool/vol"},
			{"btrfs", "property", "set", "/pool/vol", "compression", "none"},
		}},
		{"inherit", "--------c-------------", "compression=zlib", RootSettings{false, "inherit"}, [][]string{
			{"btrfs", "property", "set", "/pool/vol", "compression", ""},
		}},
		{"idempotent", "--------c-------------", "compression=lzo", RootSettings{false, "lzo"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := runCommand
			t.Cleanup(func() { runCommand = saved })
			var mutations [][]string
			runCommand = func(_ context.Context, name string, args ...string) (string, error) {
				if name == "lsattr" {
					return tc.attrs + " /pool/vol", nil
				}
				if name == "btrfs" && args[1] == "get" {
					return tc.property, nil
				}
				mutations = append(mutations, append([]string{name}, args...))
				return "", nil
			}
			if err := (&RealManager{}).ApplyRootSettings(t.Context(), "/pool/vol", tc.settings); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mutations, tc.want) {
				t.Fatalf("mutations = %q, want %q", mutations, tc.want)
			}
		})
	}
}

func TestRootSettingsErrors(t *testing.T) {
	saved := runCommand
	t.Cleanup(func() { runCommand = saved })
	for _, fail := range []string{"lsattr", "get", "set", "chattr"} {
		t.Run(fail, func(t *testing.T) {
			runCommand = func(_ context.Context, name string, args ...string) (string, error) {
				if name == fail || (name == "btrfs" && args[1] == fail) {
					return "", errors.New("unsupported")
				}
				if name == "lsattr" {
					return "--------c------------- /pool/vol", nil
				}
				return "compression=zstd", nil
			}
			err := (&RealManager{}).ApplyRootSettings(t.Context(), "/pool/vol", RootSettings{true, "none"})
			if err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("expected contextual command error, got %v", err)
			}
		})
	}
	runCommand = func(context.Context, string, ...string) (string, error) { return "", nil }
	if _, err := (&RealManager{}).GetRootSettings(t.Context(), "/pool/vol"); err == nil {
		t.Fatal("malformed lsattr output should fail")
	}
	if err := (&RealManager{}).ApplyRootSettings(t.Context(), "/pool/vol", RootSettings{false, "zstd:5"}); err == nil {
		t.Fatal("unsupported compression must fail before executing commands")
	}
}

func TestCreateSubvolumeNocowClearsInheritedCompression(t *testing.T) {
	saved := runCommand
	t.Cleanup(func() { runCommand = saved })
	compressionDisabled := false
	runCommand = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "lsattr":
			return "--------c------------- /pool/vol", nil
		case "chattr":
			if !compressionDisabled {
				t.Error("NOCOW was applied before clearing inherited compression")
			}
		case "btrfs":
			if args[0] == "property" && args[1] == "get" {
				return "compression=zstd", nil
			}
			if args[0] == "property" && args[1] == "set" && args[len(args)-1] == "none" {
				compressionDisabled = true
			}
		}
		return "", nil
	}
	if err := (&RealManager{}).CreateSubvolume(t.Context(), "/pool/vol", CreateSubvolumeOptions{Nodatacow: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSubvolumeNocowFailureCleansUp(t *testing.T) {
	saved := runCommand
	t.Cleanup(func() { runCommand = saved })
	deleted := false
	runCommand = func(_ context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "lsattr":
			return "---------------------- /pool/vol", nil
		case name == "chattr":
			return "", errors.New("permission denied")
		case name == "btrfs" && args[0] == "subvolume" && args[1] == "delete":
			deleted = true
		}
		return "", nil
	}
	if err := (&RealManager{}).CreateSubvolume(t.Context(), "/pool/vol", CreateSubvolumeOptions{Nodatacow: true}); err == nil {
		t.Fatal("attribute failure should fail creation")
	}
	if !deleted {
		t.Fatal("failed NOCOW application left an orphan subvolume")
	}
}
