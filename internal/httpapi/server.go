package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"jellymesh/internal/config"
)

type Server struct {
	config    config.Config
	startedAt time.Time
}

func New(cfg config.Config) *Server {
	return &Server{
		config:    cfg,
		startedAt: time.Now().UTC(),
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.health)
	mux.HandleFunc("/readyz", server.ready)
	return mux
}

func (server *Server) health(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	writeJSON(response, http.StatusOK, map[string]any{
		"status":             "ok",
		"service":            "jellymesh",
		"federation_enabled": server.config.FederationEnabled,
		"uptime_seconds":     int(time.Since(server.startedAt).Seconds()),
	})
}

func (server *Server) ready(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	writeJSON(response, http.StatusOK, map[string]any{
		"status":  "ready",
		"service": "jellymesh",
		"node":    server.config.NodeName,
	})
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}
