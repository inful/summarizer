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
// colliding: report names carry the constant "wk" window token and the
// audience, but no date, so without it this week's run would overwrite
// last week's.
type Local struct {
	Dir string // root directory; per-period subdirectories created on each Publish
}

// NewLocal returns a disk-backed publisher rooted at dir.
func NewLocal(dir string) *Local { return &Local{Dir: dir} }

// titled gives a local-disk report its heading.
//
// The GitLab publisher puts the title in the file's frontmatter and
// strips any H1 the model wrote, because docbuilder renders the
// frontmatter title. The local publisher writes no frontmatter, so
// without this the file had no title at all: v0.3.0 taught all eight
// prompts not to write one, and this is the path that had no other
// source for it. The output was titled only when the model disobeyed
// the instruction, which is not a property to rely on.
//
// The heading is the same string the frontmatter would have carried —
// publisher.Title — so a local file and a published file with the same
// inputs are titled identically.
//
// The model's own H1 is stripped first, so the file always has exactly
// one heading: the specific one, not "# Weekly engineering update".
func titled(req Request) []byte {
	title := Title(req.Project, req.Audience, req.Period, req.Kind)
	body := stripLeadingTitle(req.Body)
	out := make([]byte, 0, len(title)+len(body)+4)
	out = append(out, '#', ' ')
	out = append(out, title...)
	out = append(out, '\n', '\n')
	return append(out, body...)
}

// Publish conforms to Publisher. Returns the absolute path as Ref;
// MRURL is empty — there is no merge request on local disk.
func (l *Local) Publish(ctx context.Context, req Request) (Result, error) {
	dir := l.Dir
	if req.Period != "" {
		dir = filepath.Join(dir, req.Period)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	target := filepath.Join(dir, req.Name)

	body := titled(req)

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
	if _, err := tmp.Write(body); err != nil {
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
	// Cancellation is honoured at the one point it can still be acted
	// on: the rename. Checking it earlier would abandon a half-written
	// temp file for no benefit, and the deferred cleanup removes it
	// anyway. Before the rename, the previous period file is still
	// intact — so a Ctrl-C leaves the good file exactly as it was and
	// the run reports the cancellation instead of a silent success.
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return Result{}, fmt.Errorf("rename tmp→target: %w", err)
	}
	return Result{Ref: target}, nil
}
