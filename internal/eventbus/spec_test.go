package eventbus

import (
	"encoding/json"
	"testing"
)

func ev(typ, subject string, payload map[string]any) Event {
	b, _ := json.Marshal(payload)
	return Event{ID: "e1", Type: typ, Subject: subject, Payload: b}
}

func TestMatchSpec_TypeAndGlob(t *testing.T) {
	m, err := MatchSpec{Type: "pr", SubjectGlob: "31*"}.compile()
	if err != nil {
		t.Fatal(err)
	}
	if !m(ev("pr", "3177", nil)) {
		t.Error("want match for pr/3177")
	}
	if m(ev("alert", "3177", nil)) {
		t.Error("type mismatch should not match")
	}
	if m(ev("pr", "2999", nil)) {
		t.Error("glob mismatch should not match")
	}
}

func TestMatchSpec_PayloadEqualsDottedAndNumber(t *testing.T) {
	m, err := MatchSpec{PayloadEquals: map[string]string{
		"author":      "alice",
		"meta.iid":    "3177",
		"meta.urgent": "true",
	}}.compile()
	if err != nil {
		t.Fatal(err)
	}
	good := ev("pr", "x", map[string]any{
		"author": "alice",
		"meta":   map[string]any{"iid": 3177, "urgent": true},
	})
	if !m(good) {
		t.Error("want match: number + bool + nested string")
	}
	bad := ev("pr", "x", map[string]any{"author": "bob", "meta": map[string]any{"iid": 3177}})
	if m(bad) {
		t.Error("author mismatch should not match")
	}
}

func TestMatchSpec_EmptyMatchesAll(t *testing.T) {
	m, _ := MatchSpec{}.compile()
	if !m(ev("anything", "", nil)) {
		t.Error("empty match should match all")
	}
}

func TestMatchSpec_BadGlob(t *testing.T) {
	if _, err := (MatchSpec{SubjectGlob: "[bad"}).compile(); err == nil {
		t.Error("want error for invalid glob")
	}
}

func TestCompileTemplate_RendersPayload(t *testing.T) {
	render, err := compileTemplate("prompt", "Review PR {{.payload.iid}} on {{.subject}}")
	if err != nil {
		t.Fatal(err)
	}
	got := render(ev("pr", "3177", map[string]any{"iid": 3177}))
	if got != "Review PR 3177 on 3177" {
		t.Errorf("got %q", got)
	}
}

func TestCompileToSubscription_Keyed(t *testing.T) {
	sub, err := HandlerSpec{
		Name:  "h1",
		Match: MatchSpec{Type: "pr"},
		Policy: PolicySpec{
			Kind:           "keyed",
			AgentID:        "agent_x",
			KeyTemplate:    "{{.subject}}",
			PromptTemplate: "do {{.subject}}",
		},
	}.compileToSubscription()
	if err != nil {
		t.Fatal(err)
	}
	k, ok := sub.Policy.(Keyed)
	if !ok {
		t.Fatalf("policy = %T, want Keyed", sub.Policy)
	}
	if k.Key(ev("pr", "3177", nil)) != "3177" {
		t.Error("key template did not render subject")
	}
	if k.Prompt(ev("pr", "3177", nil)) != "do 3177" {
		t.Error("prompt template did not render")
	}
}

func TestCompileToSubscription_Errors(t *testing.T) {
	cases := []HandlerSpec{
		{Name: "", Policy: PolicySpec{AgentID: "a"}},                              // no name
		{Name: "h", Policy: PolicySpec{Kind: "keyed", AgentID: "a"}},              // keyed, no key_template
		{Name: "h", Policy: PolicySpec{Kind: "bogus", AgentID: "a"}},              // unknown kind
		{Name: "h", Policy: PolicySpec{Kind: "stateless"}},                        // no agent_id
		{Name: "h", Policy: PolicySpec{AgentID: "a"}, Match: MatchSpec{SubjectGlob: "[x"}}, // bad glob
	}
	for i, c := range cases {
		if _, err := c.compileToSubscription(); err == nil {
			t.Errorf("case %d: want error, got nil", i)
		}
	}
}
