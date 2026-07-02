package eventbus

import (
	"encoding/json"
	"net/http"
	"time"
)

// WebhookHandler returns an http.Handler that decodes an Event from the request
// body and dispatches it. The JSON body maps directly to Event fields; id and
// type are required. The response is the per-subscription DispatchResults so the
// caller can retrieve the resolved session ids.
func (b *Bus) WebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&e); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		if e.ID == "" || e.Type == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and type are required"})
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		results := b.Dispatch(e)
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
