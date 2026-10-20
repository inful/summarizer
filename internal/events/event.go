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
	Action   string    `json:"action"`            // "opened" | "merged" | "closed" | "pushed"
	Ref      string    `json:"ref"`               // bare identifier, e.g. "#42" or "abc1234"
	Title    string    `json:"title"`             // MR/issue title or commit subject (capped)
	Author   string    `json:"author"`            // username only — no email, no display name
	URL      string    `json:"url"`               // GitLab path, e.g. "/group/proj/-/issues/42"
	Labels   []string  `json:"labels,omitempty"`  // empty for commits
	Comments int       `json:"comments"`          // count only; bodies are not carried
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated,omitempty"`

	// TeamOnly is excluded from the customer-facing prompt but included for
	// the team-facing prompt. Bodies are a primary injection vector; data
	// markers and length caps in the prompt builder are the second line of
	// defense, the first is not putting this in customer events at all.
	TeamOnly *TeamOnlyFields `json:"team_only,omitempty"`

	// Changes is the per-MR aggregate of files touched + line adds/dels.
	// Nil for issues and commits (the /changes endpoint is MR-only). When
	// present, the prompt formatter appends "+N/-M in K files" so the
	// model can distinguish a 30-file refactor from a 5-line bug fix.
	Changes *gitlab.ChangesSummary `json:"changes,omitempty"`
}

type TeamOnlyFields struct {
	Description string `json:"description"` // MR/issue body, capped to TeamDescriptionCap runes
}