package atlassian

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTaskTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{URL: srv.URL, User: "u", Token: "t", Version: "test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestPollTask_CompletesAfterRunning(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/task/10001" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := calls.Add(1)
		status := "RUNNING"
		if n >= 2 {
			status = "COMPLETE"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "10001", "status": status, "progress": 50})
	}))
	defer srv.Close()

	c := newTaskTestClient(t, srv)
	c.taskPollInterval = 5 * time.Millisecond
	if err := c.PollTask(context.Background(), "10001"); err != nil {
		t.Fatalf("PollTask: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected at least 2 polls, got %d", calls.Load())
	}
}

func TestPollTask_FailedReportsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "7", "status": "FAILED", "message": "boom"})
	}))
	defer srv.Close()

	c := newTaskTestClient(t, srv)
	c.taskPollInterval = time.Millisecond
	err := c.PollTask(context.Background(), "7")
	if err == nil || err.Error() != `task 7 ended with status FAILED: boom` {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPollTask_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "9", "status": "ENQUEUED"})
	}))
	defer srv.Close()

	c := newTaskTestClient(t, srv)
	c.taskPollInterval = time.Millisecond
	c.taskTimeout = 10 * time.Millisecond
	if err := c.PollTask(context.Background(), "9"); err == nil {
		t.Fatal("expected timeout error")
	}
}
