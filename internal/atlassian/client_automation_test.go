package atlassian

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newTenantInfoServer returns an httptest server that answers GET
// /_edge/tenant_info with {"cloudId": cloudID} and counts how many times it
// was hit.
func newTenantInfoServer(t *testing.T, cloudID string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_edge/tenant_info" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"cloudId": cloudID})
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestAutomationURLLooksUpCloudIDAndCachesIt(t *testing.T) {
	server, hits := newTenantInfoServer(t, "abc")

	client, err := NewClient(ClientConfig{
		URL:            server.URL,
		User:           "user@example.com",
		Token:          "token",
		Version:        "test",
		AutomationBase: server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	got, err := client.AutomationURL(context.Background(), "/rule")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	want := server.URL + "/abc/rest/v1/rule"
	if got != want {
		t.Errorf("AutomationURL: got %q, want %q", got, want)
	}

	// Second call must not hit tenant_info again — cloudId is cached.
	got2, err := client.AutomationURL(context.Background(), "/rule")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if got2 != want {
		t.Errorf("AutomationURL (second call): got %q, want %q", got2, want)
	}

	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("expected tenant_info to be hit exactly once, got %d", atomic.LoadInt32(hits))
	}
}

func TestAutomationURLDefaultsToPublicAutomationBase(t *testing.T) {
	server, _ := newTenantInfoServer(t, "abc")

	client, err := NewClient(ClientConfig{
		URL:     server.URL,
		User:    "user@example.com",
		Token:   "token",
		Version: "test",
		// AutomationBase intentionally left empty — must default to the
		// public Atlassian Automation API.
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	got, err := client.AutomationURL(context.Background(), "/rule")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	want := "https://api.atlassian.com/automation/public/jira/abc/rest/v1/rule"
	if got != want {
		t.Errorf("AutomationURL: got %q, want %q", got, want)
	}
}

func TestAutomationCloudIDConfiguredOverrideSkipsLookup(t *testing.T) {
	server, hits := newTenantInfoServer(t, "should-not-be-used")

	client, err := NewClient(ClientConfig{
		URL:            server.URL,
		User:           "user@example.com",
		Token:          "token",
		Version:        "test",
		CloudID:        "configured-cloud-id",
		AutomationBase: server.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	id, err := client.CloudID(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if id != "configured-cloud-id" {
		t.Errorf("CloudID: got %q, want %q", id, "configured-cloud-id")
	}

	got, err := client.AutomationURL(context.Background(), "/rule")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	want := server.URL + "/configured-cloud-id/rest/v1/rule"
	if got != want {
		t.Errorf("AutomationURL: got %q, want %q", got, want)
	}

	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("expected tenant_info to never be hit when CloudID is configured, got %d hits", atomic.LoadInt32(hits))
	}
}

func TestAutomationNewRequestAbsoluteURLSkipsBaseURLPrefixAndKeepsAuth(t *testing.T) {
	var gotPath string
	var gotUser, gotPass string
	var gotOK bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// baseURL is deliberately a different host than the absolute request
	// target, so a bug that still prefixes baseURL would produce a
	// malformed URL rather than silently hitting the right server.
	client, err := NewClient(ClientConfig{
		URL:     "https://base.example.invalid",
		User:    "user@example.com",
		Token:   "token123",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	absoluteURL := server.URL + "/rest/v1/rule"
	resp, err := client.Do(context.Background(), "GET", absoluteURL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if gotPath != "/rest/v1/rule" {
		t.Errorf("expected server to receive path %q, got %q", "/rest/v1/rule", gotPath)
	}
	if !gotOK {
		t.Fatal("expected request to carry Basic auth")
	}
	if gotUser != "user@example.com" || gotPass != "token123" {
		t.Errorf("unexpected basic auth credentials: user=%q pass=%q", gotUser, gotPass)
	}
}
