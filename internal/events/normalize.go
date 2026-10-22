package events

import (
	"strconv"
	"strings"

	"github.com/inful/summarizer/internal/gitlab"
	"github.com/inful/summarizer/internal/textutil"
)

// Cap constants — events are sent to an LLM. Bigger is wasteful and
// amplifies the prompt-injection blast radius. Tuned to fit ~200 events
// into a 128k-context model with room for both audience prompts.
const (
	TitleCap       = 200 // MR/issue/commit subject
	DescriptionCap = 600 // body text — only emitted for the team prompt, in <data> markers
)

func MR(g gitlab.MergeRequest) NormalizedEvent {
	action := "opened"
	if g.MergedAt != nil {
		action = "merged"
	} else if g.State == "closed" {
		action = "closed"
	}
	updated := g.UpdatedAt
	if g.MergedAt != nil {
		updated = *g.MergedAt
	} else if g.ClosedAt != nil && g.MergedAt == nil {
		updated = *g.ClosedAt
	}
	return NormalizedEvent{
		Kind:                KindMR,
		Action:              action,
		Ref:                 "#" + strconv.Itoa(g.IID),
		Title:               textutil.Truncate(g.Title, TitleCap),
		Author:              g.Author.Username,
		URL:                 g.WebURL,
		Labels:              g.Labels,
		Comments:            g.UserNotesCount,
		Created:             g.CreatedAt,
		Updated:             updated,
		TeamOnlyDescription: textutil.Truncate(g.Description, DescriptionCap),
		Changes:             g.Changes,
		DiffChunks:          g.DiffChunks,
	}
}

func Issue(g gitlab.Issue) NormalizedEvent {
	action := "opened"
	if g.State == "closed" {
		action = "closed"
	}
	updated := g.UpdatedAt
	if g.ClosedAt != nil {
		updated = *g.ClosedAt
	}
	return NormalizedEvent{
		Kind:                KindIssue,
		Action:              action,
		Ref:                 "#" + strconv.Itoa(g.IID),
		Title:               textutil.Truncate(g.Title, TitleCap),
		Author:              g.Author.Username,
		URL:                 g.WebURL,
		Labels:              g.Labels,
		Comments:            g.UserNotesCount,
		Created:             g.CreatedAt,
		Updated:             updated,
		TeamOnlyDescription: textutil.Truncate(g.Description, DescriptionCap),
	}
}

func Commit(g gitlab.Commit) NormalizedEvent {
	return NormalizedEvent{
		Kind:    KindCommit,
		Action:  "pushed",
		Ref:     g.ShortID,
		Title:   textutil.Truncate(g.Title, TitleCap),
		Author:  commitAuthor(g),
		URL:     g.WebURL,
		Created: g.CreatedAt,
		Updated: g.CreatedAt,
	}
}

// commitAuthor picks a username-shaped identifier. GitLab commit records
// don't always carry a username, only a name+email; fall back to name and
// skip the email to keep the field "email-ish" data out of the prompt.
func commitAuthor(g gitlab.Commit) string {
	if g.AuthorEmail != "" {
		if at := strings.IndexByte(g.AuthorEmail, '@'); at > 0 {
			return g.AuthorEmail[:at]
		}
	}
	return g.AuthorName
}
