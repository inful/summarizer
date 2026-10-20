package events

import (
	"strings"

	"github.com/inful/summarizer/internal/gitlab"
)

// Cap constants — events are sent to an LLM. Bigger is wasteful and
// amplifies the prompt-injection blast radius. Tuned to fit ~200 events
// into a 128k-context model with room for both audience prompts.
const (
	TitleCap         = 200  // MR/issue/commit subject
	DescriptionCap   = 600  // body text — only emitted for the team prompt, in <data> markers
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
		Kind:     KindMR,
		Action:   action,
		Ref:      "#" + itoa(g.IID),
		Title:    truncate(g.Title, TitleCap),
		Author:   g.Author.Username,
		URL:      gitlabPath(g.WebURL),
		Labels:   g.Labels,
		Comments: g.UserNotesCount,
		Created:  g.CreatedAt,
		Updated:  updated,
		TeamOnly: &TeamOnlyFields{Description: truncate(g.Description, DescriptionCap)},
		Changes:  g.Changes,
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
		Kind:     KindIssue,
		Action:   action,
		Ref:      "#" + itoa(g.IID),
		Title:    truncate(g.Title, TitleCap),
		Author:   g.Author.Username,
		URL:      gitlabPath(g.WebURL),
		Labels:   g.Labels,
		Comments: g.UserNotesCount,
		Created:  g.CreatedAt,
		Updated:  updated,
		TeamOnly: &TeamOnlyFields{Description: truncate(g.Description, DescriptionCap)},
	}
}

func Commit(g gitlab.Commit) NormalizedEvent {
	return NormalizedEvent{
		Kind:    KindCommit,
		Action:  "pushed",
		Ref:     g.ShortID,
		Title:   truncate(g.Title, TitleCap),
		Author:  commitAuthor(g),
		URL:     gitlabPath(g.WebURL),
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

func gitlabPath(webURL string) string {
	webURL = strings.TrimPrefix(webURL, "https://")
	webURL = strings.TrimPrefix(webURL, "http://")
	if i := strings.IndexByte(webURL, '/'); i >= 0 {
		return webURL[i:]
	}
	return webURL
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}