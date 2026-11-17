package btrfs

import (
	"context"
	"fmt"
	"strings"
)

// RootSettings describes inheritable defaults on a subvolume's root directory.
type RootSettings struct {
	Nodatacow   bool
	Compression string
}

// Validate rejects settings which cannot be expressed per directory.
func (s RootSettings) Validate() error {
	switch s.Compression {
	case "inherit", "none", "zstd", "zlib", "lzo":
	default:
		return fmt.Errorf("invalid compression %q: expected inherit, none, zstd, zlib, or lzo (no levels)", s.Compression)
	}
	if s.Nodatacow && s.Compression != "none" {
		return fmt.Errorf("compression requires cow=true")
	}
	return nil
}

// GetRootSettings implements Manager.
func (m *RealManager) GetRootSettings(ctx context.Context, path string) (RootSettings, error) {
	attrs, err := runCommand(ctx, "lsattr", "-d", path)
	if err != nil {
		return RootSettings{}, fmt.Errorf("read root attributes %s: %w", path, err)
	}
	fields := strings.Fields(attrs)
	if len(fields) < 2 {
		return RootSettings{}, fmt.Errorf("read root attributes %s: malformed lsattr output", path)
	}
	property, err := runCommand(ctx, "btrfs", "property", "get", path, "compression")
	if err != nil {
		return RootSettings{}, fmt.Errorf("read root compression %s: %w", path, err)
	}
	settings := RootSettings{
		Nodatacow:   strings.Contains(fields[0], "C"),
		Compression: strings.TrimPrefix(strings.TrimSpace(property), "compression="),
	}
	if settings.Compression == "" {
		settings.Compression = "inherit"
	}
	if settings.Nodatacow || strings.Contains(fields[0], "m") {
		settings.Compression = "none"
	}
	if err := settings.Validate(); err != nil {
		return RootSettings{}, fmt.Errorf("read root settings %s: %w", path, err)
	}
	return settings, nil
}

// ApplyRootSettings implements Manager.
func (m *RealManager) ApplyRootSettings(ctx context.Context, path string, settings RootSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	current, err := m.GetRootSettings(ctx, path)
	if err != nil {
		return err
	}
	setCompression := func(value string) error {
		if value == "inherit" {
			value = ""
		}
		if _, err := runCommand(ctx, "btrfs", "property", "set", path, "compression", value); err != nil {
			return fmt.Errorf("set root compression %s: %w", path, err)
		}
		return nil
	}
	// NOCOW conflicts with both COMPRESS and NOCOMPRESS: remove the property, not just the algorithm.
	if settings.Nodatacow && !current.Nodatacow && current.Compression != "inherit" {
		if err := setCompression("inherit"); err != nil {
			return err
		}
	}
	if current.Nodatacow != settings.Nodatacow {
		flag := "-C"
		if settings.Nodatacow {
			flag = "+C"
		}
		if _, err := runCommand(ctx, "chattr", flag, path); err != nil {
			return fmt.Errorf("set root cow %s: %w", path, err)
		}
	}
	if !settings.Nodatacow && (current.Compression != settings.Compression || current.Nodatacow) {
		return setCompression(settings.Compression)
	}
	return nil
}
