package btrfs

import (
	"context"
	"fmt"
)

// Receive does not restore root inode flags or remove properties inherited from its destination.
func (m *RealManager) finalizeReceivedRoot(ctx context.Context, frozenSource, dst string, readonly bool) error {
	settings, err := m.GetRootSettings(ctx, frozenSource)
	if err != nil {
		return fmt.Errorf("read transferred source defaults: %w", err)
	}
	current, err := m.inspectRootSettings(ctx, dst)
	if err != nil {
		return err
	}
	if readonly && current.Nodatacow == settings.Nodatacow && current.compressionMatches(settings.Compression) {
		return nil
	}
	// Clearing received_uuid is required before modifying a received readonly snapshot.
	if _, err := runCommand(ctx, "btrfs", "property", "set", "-f", dst, "ro", "false"); err != nil {
		return fmt.Errorf("make received subvolume writable %s: %w", dst, err)
	}
	if err := m.ApplyRootSettings(ctx, dst, settings); err != nil {
		return fmt.Errorf("restore received root defaults %s: %w", dst, err)
	}
	if readonly {
		if _, err := runCommand(ctx, "btrfs", "property", "set", dst, "ro", "true"); err != nil {
			return fmt.Errorf("seal received snapshot %s: %w", dst, err)
		}
	}
	return nil
}
