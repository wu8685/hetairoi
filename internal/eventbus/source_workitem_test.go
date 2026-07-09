package eventbus

import "testing"

func TestParseWorkItems_Shapes(t *testing.T) {
	cases := map[string]string{
		"bare array":   `[{"serialNumber":"REQ-1"},{"serialNumber":"REQ-2"}]`,
		"data wrapper":  `{"data":[{"serialNumber":"REQ-1"},{"serialNumber":"REQ-2"}]}`,
		"items wrapper": `{"items":[{"serialNumber":"REQ-1"},{"serialNumber":"REQ-2"}]}`,
		"records":       `{"records":[{"serialNumber":"REQ-1"},{"serialNumber":"REQ-2"}]}`,
	}
	for name, raw := range cases {
		items, err := parseWorkItems([]byte(raw))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(items) != 2 {
			t.Errorf("%s: got %d items", name, len(items))
		}
	}
	if _, err := parseWorkItems([]byte(`{"nope":1}`)); err == nil {
		t.Error("want error for unrecognized shape")
	}
}

func TestPickField(t *testing.T) {
	it := map[string]any{"serialNumber": "REQ-42", "gmtModified": float64(1700000000000), "status": "OPEN"}
	if got := pickField(it, "", workItemIDCandidates); got != "REQ-42" {
		t.Errorf("default id = %q", got)
	}
	if got := pickField(it, "", workItemVersionCandidates); got != "1700000000000" {
		t.Errorf("default version (gmtModified wins) = %q", got)
	}
	if got := pickField(it, "status", nil); got != "OPEN" {
		t.Errorf("override = %q", got)
	}
	if got := pickField(it, "missing", nil); got != "" {
		t.Errorf("absent override should be empty, got %q", got)
	}
}

func TestWorkItemSource_FetchBuildsVersionedEvents(t *testing.T) {
	// Exercise the event-building path without spawning workitem by parsing a fixed
	// payload through the same field logic the Fetch uses.
	items, _ := parseWorkItems([]byte(`[{"serialNumber":"REQ-7","status":"DOING"}]`))
	s := WorkItemSource{VersionField: "status"}
	id := pickField(items[0], s.IDField, workItemIDCandidates)
	ver := pickField(items[0], s.VersionField, workItemVersionCandidates)
	if id != "REQ-7" || ver != "DOING" {
		t.Fatalf("id=%q ver=%q", id, ver)
	}
}

func TestBuildFetch_WorkItem(t *testing.T) {
	if _, err := buildFetch(SourceSpec{Type: "workitem", Space: "W123"}, t.TempDir()); err != nil {
		t.Errorf("valid workitem(space): %v", err)
	}
	if _, err := buildFetch(SourceSpec{Type: "workitem", Project: "P1"}, t.TempDir()); err != nil {
		t.Errorf("valid workitem(project): %v", err)
	}
	if _, err := buildFetch(SourceSpec{Type: "workitem"}, t.TempDir()); err == nil {
		t.Error("workitem without space/project should error")
	}
}
