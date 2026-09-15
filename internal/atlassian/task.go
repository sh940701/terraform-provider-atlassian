package atlassian

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultTaskPollInterval = 3 * time.Second
	defaultTaskTimeout      = 5 * time.Minute
)

// taskProgress is the response from GET /rest/api/3/task/{taskId}.
// Status values: ENQUEUED, RUNNING, COMPLETE, FAILED, CANCEL_REQUESTED, CANCELLED, DEAD.
type taskProgress struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// PollTask polls a Jira long-running task (e.g. the taskId returned by
// POST /rest/api/3/workflows/update) until it reaches a terminal status.
// COMPLETE returns nil; FAILED / CANCELLED / CANCEL_REQUESTED / DEAD return an error carrying
// the task message. Poll interval and timeout are overridable per client
// (unexported — tests shorten them).
func (c *Client) PollTask(ctx context.Context, taskID string) error {
	interval := c.taskPollInterval
	if interval <= 0 {
		interval = defaultTaskPollInterval
	}
	timeout := c.taskTimeout
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}

	path := "/rest/api/3/task/" + PathEscape(taskID)
	deadline := time.Now().Add(timeout)
	for {
		var task taskProgress
		if err := c.Get(ctx, path, &task); err != nil {
			return fmt.Errorf("polling task %s: %w", taskID, err)
		}

		switch task.Status {
		case "COMPLETE":
			return nil
		case "FAILED", "CANCELLED", "CANCEL_REQUESTED", "DEAD":
			return fmt.Errorf("task %s ended with status %s: %s", taskID, task.Status, task.Message)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("task %s still %s after %s", taskID, task.Status, timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
