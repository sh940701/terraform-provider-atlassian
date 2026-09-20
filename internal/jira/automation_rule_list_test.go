package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

func TestListAutomationRuleSummariesPagesAndRequiresData(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/abc/rest/v1/rule/summary" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "2" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data":  []map[string]string{{"uuid": "u2", "name": "keep-me", "state": "ENABLED"}},
				"links": map[string]string{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data":  []map[string]string{{"uuid": "u1", "name": "tf-acc-test-one", "state": "DISABLED"}},
			"links": map[string]string{"next": "http://" + r.Host + "/abc/rest/v1/rule/summary?cursor=2"},
		})
	}))
	t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{
		URL:            server.URL,
		User:           "user@example.com",
		Token:          "token",
		CloudID:        "abc",
		AutomationBase: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %s", err)
	}

	got, err := ListAutomationRuleSummaries(context.Background(), client)
	if err != nil {
		t.Fatalf("listAutomationRuleSummaries: %s", err)
	}
	if len(got) != 2 || got[0].UUID != "u1" || got[1].UUID != "u2" {
		t.Fatalf("summaries: got %#v", got)
	}
}

func TestListAutomationRuleSummariesErrorsWithoutDataKey(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"rules": []map[string]string{{"uuid": "u1", "name": "tf-acc-test-one"}},
		})
	}))
	t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{
		URL:            server.URL,
		User:           "user@example.com",
		Token:          "token",
		CloudID:        "abc",
		AutomationBase: server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient: %s", err)
	}

	_, err = ListAutomationRuleSummaries(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "missing data key") {
		t.Fatalf("expected missing data key error, got %v", err)
	}
}
