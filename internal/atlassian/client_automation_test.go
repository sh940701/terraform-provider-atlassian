package atlassian

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// malformed URL rather than silently hitting the right server. The
	// target host must still be an *allowed* absolute-URL host, so it is
	// configured as AutomationBase.
	client, err := NewClient(ClientConfig{
		URL:            "https://base.example.invalid",
		User:           "user@example.com",
		Token:          "token123",
		Version:        "test",
		AutomationBase: server.URL,
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

// TestAutomationCloudIDDoesNotCacheFailedLookupAndRetries covers review
// finding #2: a failed tenant_info lookup must not be cached forever — the
// next call has to retry.
//
// The failing response here is 400 Bad Request rather than the reviewer's
// suggested 500: Client.Do already retries 429/5xx internally with
// exponential backoff (up to 5 retries, delays up to 30s each), so a mock
// that returns 500 once and 200 afterwards would usually be swallowed by
// that internal retry within a single CloudID call — the first call would
// quietly succeed a second or two later instead of surfacing an error, and
// the test would take tens of seconds. 400 exercises the exact caching bug
// (CloudID must not remember the failure) without depending on, or being
// slowed down by, the unrelated 5xx retry path.
func TestAutomationCloudIDDoesNotCacheFailedLookupAndRetries(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_edge/tenant_info" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"cloudId": "abc"})
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		URL:     server.URL,
		User:    "user@example.com",
		Token:   "token",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if _, err := client.CloudID(context.Background()); err == nil {
		t.Fatal("expected an error on the first (failing) lookup")
	}

	id, err := client.CloudID(context.Background())
	if err != nil {
		t.Fatalf("expected the second lookup to retry and succeed, got error: %s", err)
	}
	if id != "abc" {
		t.Errorf("CloudID: got %q, want %q", id, "abc")
	}

	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected tenant_info to be hit exactly twice (one failed lookup + one retry), got %d", hits)
	}
}

// TestAutomationNewRequestRejectsForeignAbsoluteURL covers review finding
// #4: an absolute URL is only sent if its scheme+host matches the
// configured site (baseURL) or the configured AutomationBase. Anything else
// must be rejected before Basic auth is attached or any network call is
// made.
func TestAutomationNewRequestRejectsForeignAbsoluteURL(t *testing.T) {
	var foreignHits int32
	foreignServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&foreignHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer foreignServer.Close()

	siteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer siteServer.Close()

	client, err := NewClient(ClientConfig{
		URL:            siteServer.URL,
		User:           "user@example.com",
		Token:          "token",
		Version:        "test",
		AutomationBase: siteServer.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	_, err = client.Do(context.Background(), "GET", foreignServer.URL+"/steal", nil)
	if err == nil {
		t.Fatal("expected an error for an absolute URL whose host is neither the configured site nor the Automation API")
	}
	if !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("expected error to explain the host was disallowed, got: %s", err)
	}
	if atomic.LoadInt32(&foreignHits) != 0 {
		t.Errorf("expected the foreign host to never receive the request, got %d hits", foreignHits)
	}
}
