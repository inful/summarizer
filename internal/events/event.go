// Package events defines the NormalizedEvent type — the lingua franca between
// the GitLab client and the LLM. GitLab JSON never crosses this boundary;
// adapters in package gitlab map into NormalizedEvent, and the prompt builder
// reads from it.
package events

import (
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

// Kind identifies the source record type.
type Kind string

const (
	KindMR     Kind = "mr"
	KindIssue  Kind = "issue"
	KindCommit Kind = "commit"
)

// NormalizedEvent is the single shape the prompt builder consumes. Fields are
// chosen for LLM context efficiency and prompt-injection hygiene: free-text
// GitLab fields are kept under a tight length cap and never passed straight
// through without sanitization.
type NormalizedEvent struct {
	Kind     Kind      `json:"kind"`
	Action   string    `json:"action"`           // "opened" | "merged" | "closed" | "pushed"
	Ref      string    `json:"ref"`              // bare identifier, e.g. "#42" or "abc1234"
	Title    string    `json:"title"`            // MR/issue title or commit subject (capped)
	Author   string    `json:"author"`           // username only — no email, no display name
	URL      string    `json:"url"`              // GitLab path, e.g. "/group/proj/-/issues/42"
	Labels   []string  `json:"labels,omitempty"` // empty for commits
	Comments int       `json:"comments"`         // count only; bodies are not carried
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated,omitempty"`

	// TeamOnlyDescription is the MR or issue body, excluded from the
	// customer-facing prompt and included for the team-facing one.
	// Bodies are a primary injection vector; data markers and length
	// caps in the prompt builder are the second line of defence, the
	// first is not putting this in customer events at all.
	//
	// A plain string, not a pointer to a one-field struct: it was
	// constructed unconditionally and read only when non-empty, so nil
	// and "set to nothing" were indistinguishable and the nil check in
	// the renderer could never fire.
	TeamOnlyDescription string `json:"team_only_description,omitempty"`

	// Changes is the per-MR aggregate of files touched + line adds/dels.
	// Nil for issues and commits (the /changes endpoint is MR-only). When
	// present, the prompt formatter appends "+N/-M in K files" so the
	// model can distinguish a 30-file refactor from a 5-line bug fix.
	Changes *gitlab.ChangesSummary `json:"changes,omitempty"`

	// DiffChunks is the per-file diff text for the MR. Populated only
	// when the operator passes --include-diffs; nil otherwise. Diff bodies
	// are a high-blast-radius prompt-injection surface, so the team
	// prompt instructions tell the model to "infer what the MR does from
	// the diff" rather than reproduce code into the report.
	DiffChunks []gitlab.DiffChunk `json:"diff_chunks,omitempty"`

	// Confidential carries GitLab's own "not for everyone" flag, which is
	// only set on issues. The customer-facing prompt drops these events
	// outright — that document is published public: true, and an issue
	// title routinely carries the very thing the flag was set to hide.
	// The team prompt keeps them; that file is internal and the team is
	// who marked the issue.
	//
	// The Epic pointer below carries the same information for the epic
	// half of the pair, which needs no field here — but see
	// prompt.Render, which drops an event when EITHER flag is set.
	Confidential bool `json:"confidential,omitempty"`

	// Epic links this event to an enclosing epic, populated by the
	// correlation step when the event's source (issue IID or MR-described
	// issue link) matches an issue on a known epic. Nil for events with
	// no epic context. Used by the prompt renderer to group events under
	// epic headers in the section that surfaces active epics.
	Epic *gitlab.Epic `json:"-"` // set by AttachEpics, read by the renderer
}
