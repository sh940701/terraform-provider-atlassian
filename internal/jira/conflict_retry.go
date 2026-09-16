package jira

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// configWriteMu serialises writes that contend for Jira's single workflow-
// configuration lock (workflow create/update, status create/update/delete).
// Terraform runs resources in parallel; Jira answers 409 "Failed to acquire
// lock" to every write that overlaps another, and a workflow write takes ~30s,
// so retrying alone cannot keep up (sandbox apply, 2026-09-17). All such
// writes inside one provider process take this lock first; retryOnConflict
// remains for contention from other clients.
var configWriteMu sync.Mutex

// withConfigLock runs fn while holding configWriteMu.
func withConfigLock(fn func() error) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	return fn()
}

// conflictRetryDelay is the base wait between retries of a write that Jira
// refused with 409 (e.g. POST /rest/api/3/statuses: "Failed to acquire lock"
// when several statuses are created in one apply). Tests shorten it.
var conflictRetryDelay = 2 * time.Second

// conflictRetryAttempts bounds retryOnConflict; the wait grows linearly.
const conflictRetryAttempts = 5

// retryOnConflict runs fn until it reports a status other than 409 or the
// attempts are exhausted, returning fn's last error.
func retryOnConflict(ctx context.Context, fn func() (int, error)) error {
	var err error
	for attempt := 1; attempt <= conflictRetryAttempts; attempt++ {
		var status int
		status, err = fn()
		if status != http.StatusConflict {
			return err
		}
		if attempt == conflictRetryAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(conflictRetryDelay * time.Duration(attempt)):
		}
	}
	return err
}
