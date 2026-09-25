package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"jellymesh/internal/config"
)

func TestHealth(t *testing.T) {
	cfg := config.Config{
		AdminListenAddress: "127.0.0.1:8091",
		DataDirectory:      "/tmp/jellymesh-test",
		NodeName:           "Walnut",
		FederationEnabled:  false,
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	New(cfg).Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if payload["federation_enabled"] != false {
		t.Fatalf("federation should be disabled: %#v", payload)
	}
}

func TestReady(t *testing.T) {
	cfg := config.Config{NodeName: "Walnut"}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	New(cfg).Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["node"] != "Walnut" {
		t.Fatalf("unexpected node: %#v", payload)
	}
}

func TestHealthRejectsNonGet(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	response := httptest.NewRecorder()

	New(config.Config{}).Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status: %d", response.Code)
	}
}
