package publisher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrExists signals the target file already exists and Force was not set.
var ErrExists = errors.New("output file already exists")

// Local writes Markdown reports to a local directory under Dir. The flow:
//  1. Refuse if dir/name exists and !Force
//  2. mkdir -p dir
//  3. write to dir/.name.tmp-<pid>-<rand>
//  4. rename tmp -> dir/name
//
// Rename is atomic on POSIX (same filesystem); a crash before step 4
// leaves no half-written file in the target path.
type Local struct {
	Dir string // root directory; created on each Publish
}

// NewLocal returns a disk-backed publisher rooted at dir.
func NewLocal(dir string) *Local { return &Local{Dir: dir} }

// Publish conforms to Publisher. Returns the absolute path as Ref;
// Branch/MRURL are empty; Skipped is true when refuse-first declined.
func (l *Local) Publish(req Request) (Result, error) {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir %s: %w", l.Dir, err)
	}
	target := filepath.Join(l.Dir, req.Name)
	if !req.Force {
		if _, err := os.Stat(target); err == nil {
			return Result{Ref: target, Skipped: true}, fmt.Errorf("%w: %s", ErrExists, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("stat %s: %w", target, err)
		}
	}
	tmp, err := os.CreateTemp(l.Dir, "."+req.Name+".tmp-*")
	if err != nil {
		return Result{}, fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure cleanup on any failure path before the rename.
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(req.Body); err != nil {
		_ = tmp.Close() // best-effort cleanup; the Write error is what we report
		return Result{}, fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Result{}, fmt.Errorf("fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return Result{}, fmt.Errorf("rename tmp→target: %w", err)
	}
	return Result{Ref: target}, nil
}
