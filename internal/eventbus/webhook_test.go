package eventbus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhook_DispatchAndShape(t *testing.T) {
	f := newFake()
	b := New(f, t.TempDir(), 8)
	_ = b.Register(Subscription{Name: "s", Match: func(e Event) bool { return e.Type == "alert" }, Policy: stateless("handle")})
	h := b.WebhookHandler()

	// well-formed → 200, dispatched, session id in response
	body := `{"id":"e1","type":"alert","subject":"svc-a","payload":{"sev":"high"}}`
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			SessionID string `json:"session_id"`
		} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 1 || resp.Results[0].SessionID == "" {
		t.Fatalf("expected one result with a session id, got %s", w.Body.String())
	}
	if f.sentCount() != 1 {
		t.Fatalf("sent %d, want 1", f.sentCount())
	}

	// malformed body → 400, nothing dispatched
	req2 := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`not json`))
	w2 := httptest.NewRecorder()
	h(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d, want 400", w2.Code)
	}

	// missing required fields → 400
	req3 := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"type":"alert"}`))
	w3 := httptest.NewRecorder()
	h(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("missing id status=%d, want 400", w3.Code)
	}
	if f.sentCount() != 1 {
		t.Fatalf("malformed/invalid events must not dispatch; sent=%d", f.sentCount())
	}
}
