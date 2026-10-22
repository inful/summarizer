package events

import (
	"testing"
	"time"

	"github.com/inful/summarizer/internal/gitlab"
)

func TestMR_ActionAndRefs(t *testing.T) {
	merged := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	g := gitlab.MergeRequest{
		IID: 42, Title: "Add login button",
		Description:    "Implements the new login form per design v3.",
		State:          "merged",
		Author:         gitlab.User{Username: "alice"},
		WebURL:         "https://gitlab.example.com/group/proj/-/merge_requests/42",
		Labels:         []string{"feature", "frontend"},
		UserNotesCount: 3,
		CreatedAt:      time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		UpdatedAt:      merged,
		MergedAt:       &merged,
		Changes:        &gitlab.ChangesSummary{Files: 5, Additions: 312, Deletions: 47},
	}
	e := MR(g)
	if e.Kind != KindMR || e.Action != "merged" || e.Ref != "#42" || e.Author != "alice" {
		t.Fatalf("unexpected MR event: %+v", e)
	}
	if e.URL != "https://gitlab.example.com/group/proj/-/merge_requests/42" {
		t.Fatalf("URL should be the full web_url (incl. host), got %q", e.URL)
	}
	if e.Comments != 3 || len(e.Labels) != 2 {
		t.Fatalf("comments/labels lost: %+v", e)
	}
	if e.TeamOnlyDescription == "" {
		t.Fatalf("team-only description not carried: %+v", e)
	}
	if e.Changes == nil || e.Changes.Files != 5 || e.Changes.Additions != 312 || e.Changes.Deletions != 47 {
		t.Fatalf("changes not carried through normalize: %+v", e.Changes)
	}
}

func TestIssue_ClosedCarriesClosedTime(t *testing.T) {
	closed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	g := gitlab.Issue{
		IID: 7, Title: "Typo in docs",
		State:    "closed",
		Author:   gitlab.User{Username: "bob"},
		WebURL:   "https://gitlab.example.com/group/proj/-/issues/7",
		ClosedAt: &closed,
	}
	e := Issue(g)
	if e.Action != "closed" {
		t.Fatalf("expected closed, got %q", e.Action)
	}
	if !e.Updated.Equal(closed) {
		t.Fatalf("Updated should equal closed_at, got %v", e.Updated)
	}
}

func TestCommit_AuthorFromEmail(t *testing.T) {
	g := gitlab.Commit{
		ShortID: "abc1234", Title: "wip",
		AuthorName: "Carol Q.", AuthorEmail: "carol@example.com",
		WebURL:    "https://gitlab.example.com/group/proj/-/commit/abc1234",
		CreatedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
	}
	e := Commit(g)
	if e.Author != "carol" {
		t.Fatalf("commit author should be username-shaped, got %q", e.Author)
	}
}
