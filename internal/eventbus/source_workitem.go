package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// WorkItemSource is a built-in poll Source over the workitem CLI: it lists work items in
// a space/project and emits one event per item, keyed by the item's mutable
// version so an unchanged item is deduped and a changed one re-triggers.
//
// workitem's JSON schema is not fixed across deployments, so the id/version field
// names are configurable (with sensible default candidates). Validate the field
// mapping against your own space the first time you wire it.
type WorkItemSource struct {
	Bin        string   // workitem binary (default "workitem")
	Space      string   // -s <workspaceId>
	Project    string   // -p <projectId>
	Scope      string   // --scope (default "personal" — my items)
	Belong     string   // --belong (Workitem/Task/Req/Bug/...); empty = default
	StatusList []string // --status-list filter
	PageSize   int      // --page-size (default 50)
	EventType  string   // emitted Event.Type (default "workitem")

	// IDField / VersionField override the JSON keys used for the dedup identity.
	// Empty falls back to the default candidate lists below.
	IDField      string
	VersionField string
}

var (
	workItemIDCandidates      = []string{"serialNumber", "serialNo", "identifier", "id", "workitemId"}
	workItemVersionCandidates = []string{"gmtModified", "updatedAt", "modified", "lastModified", "status"}
)

func (s WorkItemSource) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return "workitem"
}

func (s WorkItemSource) evType() string {
	if s.EventType != "" {
		return s.EventType
	}
	return "workitem"
}

// Fetch implements FetchFunc: list work items and build one event per item.
func (s WorkItemSource) Fetch(ctx context.Context) ([]Event, error) {
	out, err := s.run(ctx)
	if err != nil {
		return nil, err
	}
	items, err := parseWorkItems(out)
	if err != nil {
		return nil, err
	}
	var evs []Event
	for _, it := range items {
		id := pickField(it, s.IDField, workItemIDCandidates)
		if id == "" {
			continue // can't form a stable identity — skip rather than churn
		}
		ver := pickField(it, s.VersionField, workItemVersionCandidates)
		payload, _ := json.Marshal(it)
		evs = append(evs, Event{
			ID:      fmt.Sprintf("workitem-%s-%s", id, ver),
			Type:    s.evType(),
			Subject: id,
			Payload: payload,
			Source:  "workitem:" + s.Space + s.Project,
			Time:    time.Now(),
		})
	}
	return evs, nil
}

func (s WorkItemSource) run(ctx context.Context) ([]byte, error) {
	args := []string{"workitem", "list", "-o", "json"}
	if s.Space != "" {
		args = append(args, "-s", s.Space)
	}
	if s.Project != "" {
		args = append(args, "-p", s.Project)
	}
	scope := s.Scope
	if scope == "" {
		scope = "personal"
	}
	args = append(args, "--scope", scope)
	if s.Belong != "" {
		args = append(args, "--belong", s.Belong)
	}
	for _, st := range s.StatusList {
		args = append(args, "--status-list", st)
	}
	ps := s.PageSize
	if ps <= 0 {
		ps = 50
	}
	args = append(args, "--page-size", strconv.Itoa(ps))

	cmd := exec.CommandContext(ctx, s.bin(), args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s %v: %s", s.bin(), args, string(ee.Stderr))
		}
		return nil, err
	}
	return out, nil
}

// parseWorkItems tolerates the common workitem response shapes: a bare array, or an
// object wrapping the array under data/items/list/records.
func parseWorkItems(raw []byte) ([]map[string]any, error) {
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse workitem list: %w", err)
	}
	for _, key := range []string{"data", "items", "list", "records", "result"} {
		if v, ok := obj[key]; ok {
			if err := json.Unmarshal(v, &arr); err == nil {
				return arr, nil
			}
		}
	}
	return nil, fmt.Errorf("parse workitem list: no recognizable item array")
}

// pickField returns the stringified value of override (if set and present), else
// the first present candidate. Empty when none match.
func pickField(item map[string]any, override string, candidates []string) string {
	if override != "" {
		if v, ok := item[override]; ok {
			return stringify(v)
		}
		return ""
	}
	for _, k := range candidates {
		if v, ok := item[k]; ok && v != nil {
			return stringify(v)
		}
	}
	return ""
}
