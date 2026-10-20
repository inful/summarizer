// Package writer writes Markdown reports to disk atomically. Write() refuses
// to overwrite an existing file unless the caller opts in via force=true —
// a cron run that lands twice for the same window must not silently clobber
// the previous good output.
package writer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrExists signals the target file already exists and force was not set.
var ErrExists = errors.New("output file already exists")

// Write atomically writes body to dir/name. The flow is:
//  1. refuse if dir/name exists and !force
//  2. mkdir -p dir
//  3. write to dir/.name.tmp-<pid>-<rand>
//  4. rename tmp -> dir/name
// Rename is atomic on POSIX (same filesystem); a crash before step 4 leaves
// no half-written file in the target path.
type Writer struct{}

func New() *Writer { return &Writer{} }

// Write performs the atomic write. Returns ErrExists if force=false and the
// target already exists. Returns the absolute path that was written.
func (w *Writer) Write(dir, name string, body []byte, force bool) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	target := filepath.Join(dir, name)
	if !force {
		if _, err := os.Stat(target); err == nil {
			return "", fmt.Errorf("%w: %s", ErrExists, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", target, err)
		}
	}
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure cleanup on any failure path before the rename.
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("rename tmp→target: %w", err)
	}
	return target, nil
}