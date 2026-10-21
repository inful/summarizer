package gitlab

import (
	"encoding/json"
	"testing"
	"time"
)

// TestEpic_UnmarshalsLabels: GitLab's GET /groups/:id/epics returns
// `"labels": ["org::ACME", "priority::high"]`. The struct must round-
// trip that field so the per-customer rollup can read it.
func TestEpic_UnmarshalsLabels(t *testing.T) {
	body := `{
		"id": 1, "iid": 4, "group_id": 7, "parent_id": null, "parent_iid": null,
		"title": "ACME migration", "description": "",
		"state": "opened", "confidential": false,
		"web_url": "u/4",
		"start_date": null, "due_date": null,
		"created_at": "2026-10-13T10:00:00.000Z",
		"updated_at": "2026-10-19T10:00:00.000Z",
		"closed_at": null,
		"labels": ["org::ACME", "priority::high"]
	}`
	var e Epic
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.IID != 4 || e.Title != "ACME migration" {
		t.Fatalf("unmarshal got %+v", e)
	}
	want := []string{"org::ACME", "priority::high"}
	if len(e.Labels) != 2 {
		t.Fatalf("labels: got %v, want %v", e.Labels, want)
	}
	for i, w := range want {
		if e.Labels[i] != w {
			t.Errorf("labels[%d] = %q, want %q", i, e.Labels[i], w)
		}
	}
	// Round-trip preserves the labels.
	out, _ := json.Marshal(e)
	var back Epic
	_ = json.Unmarshal(out, &back)
	if len(back.Labels) != 2 || back.Labels[0] != "org::ACME" {
		t.Errorf("round-trip lost labels: %+v", back.Labels)
	}
}

// TestEpic_LabelsEmptyDefaultsToNil: an epic with no labels comes back
// as a nil/empty slice so the policy package's `for _, l := range
// labels` walk just doesn't fire. Some installs (older GitLab
// versions) omit the field entirely.
func TestEpic_LabelsEmptyDefaultsToNil(t *testing.T) {
	body := `{"iid": 1, "title": "x", "state": "opened", "web_url": "u",
		"created_at": "2026-10-13T10:00:00.000Z", "updated_at": "2026-10-13T10:00:00.000Z"}`
	var e Epic
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(e.Labels) != 0 {
		t.Errorf("missing labels field should be empty/nil, got %v", e.Labels)
	}
	// and CreatedAt still parses correctly so a label-less epic
	// remains useful for the active-epic filter.
	if e.CreatedAt.IsZero() {
		t.Error("CreatedAt lost on label-less unmarshal")
	}
	_ = time.Time{}
}
