package jira

import (
	"testing"
	"time"
)

// Shortens the 409 retry delay for the package-external tests (they run in
// the same test binary). Set once; the zero-cost default stays 3s in prod.
func init() {
	if testing.Testing() {
		workflowConflictRetryDelay = 10 * time.Millisecond
		conflictRetryDelay = 10 * time.Millisecond
	}
}
