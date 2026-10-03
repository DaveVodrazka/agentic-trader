// Package fsutil holds small filesystem helpers shared by the state files.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data via a synced temp file and rename,
// so readers never observe a partially written file.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
