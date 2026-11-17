package btrfs

import (
	"context"
	"errors"
	"fmt"
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
			{"btrfs", "property", "set", "/pool/vol", "compression", ""},
			{"chattr", "+C", "/pool/vol"},
		}},
		{"disable cow from compression none", "---------------------m", "compression=", RootSettings{true, "none"}, [][]string{
			{"btrfs", "property", "set", "/pool/vol", "compression", ""},
			{"chattr", "+C", "/pool/vol"},
		}},
		{"disable cow from flag-only compression", "--------c-------------", "compression=", RootSettings{true, "none"}, [][]string{
			{"btrfs", "property", "set", "/pool/vol", "compression", ""},
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
		{"inherit from flag-only compression", "--------c-------------", "compression=", RootSettings{false, "inherit"}, [][]string{
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
			if args[0] == "property" && args[1] == "set" && args[len(args)-1] == "" {
				compressionDisabled = true
			}
		}
		return "", nil
	}
	if err := (&RealManager{}).CreateSubvolume(t.Context(), "/pool/vol", CreateSubvolumeOptions{Nodatacow: true}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRootSettingsKernelFlagConstraints(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nocow       bool
		compression string
		want        RootSettings
	}{
		{"compression to nocow", false, "zstd", RootSettings{true, "none"}},
		{"nocompress to nocow", false, "none", RootSettings{true, "none"}},
		{"inherit to nocow", false, "inherit", RootSettings{true, "none"}},
		{"nocow to compression", true, "inherit", RootSettings{false, "zstd"}},
		{"nocow to nocompress", true, "inherit", RootSettings{false, "none"}},
		{"nocow to inherit", true, "inherit", RootSettings{false, "inherit"}},
		{"nocow unchanged", true, "inherit", RootSettings{true, "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := runCommand
			t.Cleanup(func() { runCommand = saved })
			flags := &kernelRootFlags{nocow: tc.nocow, compression: tc.compression}
			runCommand = flags.run
			m := &RealManager{}
			if err := m.ApplyRootSettings(t.Context(), "/pool/vol", tc.want); err != nil {
				t.Fatal(err)
			}
			got, err := m.GetRootSettings(t.Context(), "/pool/vol")
			if err != nil || got != tc.want {
				t.Fatalf("root settings = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

type kernelRootFlags struct {
	nocow       bool
	compression string
	flagOnly    bool
}

func (k *kernelRootFlags) run(_ context.Context, name string, args ...string) (string, error) {
	switch name {
	case "lsattr":
		return k.attributes() + " /pool/vol", nil
	case "btrfs":
		if args[1] == "get" {
			if k.compression == "inherit" {
				return "compression=", nil
			}
			return "compression=" + k.compression, nil
		}
		value := args[len(args)-1]
		if k.nocow && value != "" {
			return "", errors.New("compression conflicts with NOCOW")
		}
		k.compression = value
		k.flagOnly = false
		if k.compression == "" {
			k.compression = "inherit"
		}
	case "chattr":
		if k.compression != "inherit" || k.flagOnly {
			return "", errors.New("NOCOW conflicts with COMPRESS or NOCOMPRESS")
		}
		k.nocow = args[0] == "+C"
	default:
		return "", fmt.Errorf("unexpected command %s %q", name, args)
	}
	return "", nil
}

func (k *kernelRootFlags) attributes() string {
	flags := "----------------------"
	if k.nocow {
		flags += "C"
	}
	if k.compression == "none" {
		flags += "m"
	}
	if k.flagOnly || (k.compression != "inherit" && k.compression != "none") {
		flags += "c"
	}
	return flags
}

func TestFlagOnlyCompressionTransitions(t *testing.T) {
	for _, desired := range []RootSettings{
		{Compression: "inherit"},
		{Nodatacow: true, Compression: "none"},
	} {
		t.Run(fmt.Sprintf("%+v", desired), func(t *testing.T) {
			saved := runCommand
			t.Cleanup(func() { runCommand = saved })
			flags := &kernelRootFlags{compression: "inherit", flagOnly: true}
			runCommand = flags.run
			m := &RealManager{}
			if err := m.ApplyRootSettings(t.Context(), "/pool/vol", desired); err != nil {
				t.Fatal(err)
			}
			if flags.flagOnly {
				t.Fatal("legacy COMPRESS flag was not cleared")
			}
			got, err := m.GetRootSettings(t.Context(), "/pool/vol")
			if err != nil || got != desired {
				t.Fatalf("root settings = %+v, %v; want %+v", got, err, desired)
			}
		})
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
