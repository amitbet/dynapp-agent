package shellagent

import (
	"net/http"
)

func writeDirectLocalInfoCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Access-Control-Allow-Local-Network", "true")
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) handleDirectLocalInfo(w http.ResponseWriter, r *http.Request) {
	writeDirectLocalInfoCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status, err := s.lanStatus()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	environmentID := s.Config.EnvironmentID
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"version":                 1,
		"carrier":                 "webtransport",
		"endpoint":                status["directEndpoint"],
		"lanEndpoints":            status["lanEndpoints"],
		"serverCertificateHashes": status["serverCertificateHashes"],
		"environmentId":           environmentID,
		"running":                 status["running"],
	})
}
