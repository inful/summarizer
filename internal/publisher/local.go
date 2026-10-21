package publisher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Local writes Markdown reports to a local directory under Dir, one
// subdirectory per period: <Dir>/<period>/<name>. The flow:
//  1. mkdir -p <Dir>/<period>
//  2. write to <Dir>/<period>/.name.tmp-<rand>
//  3. rename tmp -> <Dir>/<period>/<name>
//
// Rename is atomic on POSIX (same filesystem); a crash before step 3
// leaves no half-written file in the target path.
//
// There is no refuse-first. Every run inside a period targets the same
// (project, audience, period) path, so refusing would fail the second
// run of every week; overwriting in place is the intended behaviour.
//
// The period subdirectory is what keeps *different* weeks from
// colliding: report names carry the window (7d, 30d) but not the date,
// so without it this week's run would overwrite last week's.
type Local struct {
	Dir string // root directory; per-period subdirectories created on each Publish
}

// NewLocal returns a disk-backed publisher rooted at dir.
func NewLocal(dir string) *Local { return &Local{Dir: dir} }

// Publish conforms to Publisher. Returns the absolute path as Ref;
// Branch and MRURL are empty.
func (l *Local) Publish(ctx context.Context, req Request) (Result, error) {
	dir := l.Dir
	if req.Period != "" {
		dir = filepath.Join(dir, req.Period)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	target := filepath.Join(dir, req.Name)

	tmp, err := os.CreateTemp(dir, "."+req.Name+".tmp-*")
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
