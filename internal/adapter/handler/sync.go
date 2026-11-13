package handler

import (
	"net/http"

	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// SyncControl starts contact sync cycles on demand and reports the last one.
type SyncControl interface {
	Trigger() bool
	LastRun() (contactsync.RunSummary, bool)
}

// triggerSync asks for a sync cycle now (POST /internal/sync/run). Only the
// leader replica actually syncs; others report leader=false in their status.
func triggerSync(s SyncControl) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s.Trigger() {
			writeJSON(w, http.StatusAccepted, map[string]any{"triggered": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"triggered": false, "reason": "a cycle is already pending"})
	})
}

// syncStatus reports the last completed cycle (GET /internal/sync/status).
func syncStatus(s SyncControl) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		last, ok := s.LastRun()
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"status": "no cycle has completed yet"})
			return
		}
		writeJSON(w, http.StatusOK, last)
	})
}
