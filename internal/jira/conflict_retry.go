package jira

import (
	"context"
	"net/http"
	"time"
)

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
