package atlassian

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRetries           = 5
	baseDelay            = 1 * time.Second
	maxRetryDelay        = 30 * time.Second
	maxRetryAfterSeconds = 60

	// defaultAutomationBase is the public Atlassian Automation API base
	// URL. A cloud ID and a rest path are appended to it by AutomationURL.
	defaultAutomationBase = "https://api.atlassian.com/automation/public/jira"

	// tenantInfoPath resolves the site URL configured on the client to its
	// Atlassian cloud ID.
	tenantInfoPath = "/_edge/tenant_info"
)

// Client is the Atlassian Cloud API client.
type Client struct {
	baseURL          string
	user             string
	token            string
	version          string
	httpClient       *http.Client
	automationBase   string
	baseOrigin       string // scheme://host of baseURL
	automationOrigin string // scheme://host of automationBase

	// Long-running task polling (see task.go). Zero means package defaults.
	taskPollInterval time.Duration
	taskTimeout      time.Duration

	// cloudID caching. configuredCloudID, when non-empty, is returned as-is
	// and the tenant_info lookup is never performed. Otherwise cloudIDMu
	// guards cloudIDValue, which caches only a *successful* lookup: a
	// failed lookup is never stored, so the next call retries it. The lock
	// is held across the lookup itself, so concurrent callers either see
	// the cached value or wait for the single in-flight lookup rather than
	// firing duplicate requests.
	configuredCloudID string
	cloudIDMu         sync.Mutex
	cloudIDValue      string
}

// ClientConfig holds the configuration for creating a new Client.
type ClientConfig struct {
	URL     string
	User    string
	Token   string
	Version string
	// ResponseHeaderTimeout is how long to wait for response headers before
	// cancelling the request. Defaults to 30s. Set a shorter value in tests.
	ResponseHeaderTimeout time.Duration
	// CloudID, when set, is used as-is by CloudID/AutomationURL and skips
	// the /_edge/tenant_info lookup entirely. Optional.
	CloudID string
	// AutomationBase overrides the Automation API base URL used by
	// AutomationURL. Defaults to the public Atlassian Automation API.
	// Tests point this at an httptest server. Optional.
	AutomationBase string
}

// NewClient creates a new Atlassian API client.
// Explicit config values take precedence over environment variables.
func NewClient(config ClientConfig) (*Client, error) {
	u := config.URL
	if u == "" {
		u = os.Getenv("ATLASSIAN_URL")
	}

	user := config.User
	if user == "" {
		user = os.Getenv("ATLASSIAN_USER")
	}

	token := config.Token
	if token == "" {
		token = os.Getenv("ATLASSIAN_TOKEN")
	}

	if u == "" {
		return nil, fmt.Errorf("atlassian URL is required (set url in provider config or ATLASSIAN_URL env var)")
	}
	if user == "" {
		return nil, fmt.Errorf("atlassian user is required (set user in provider config or ATLASSIAN_USER env var)")
	}
	if token == "" {
		return nil, fmt.Errorf("atlassian token is required (set token in provider config or ATLASSIAN_TOKEN env var)")
	}

	baseURL := strings.TrimRight(u, "/")

	responseHeaderTimeout := 30 * time.Second
	if config.ResponseHeaderTimeout > 0 {
		responseHeaderTimeout = config.ResponseHeaderTimeout
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout

	automationBase := config.AutomationBase
	if automationBase == "" {
		automationBase = defaultAutomationBase
	}
	automationBase = strings.TrimRight(automationBase, "/")

	return &Client{
		baseURL:          baseURL,
		user:             user,
		token:            token,
		version:          config.Version,
		automationBase:   automationBase,
		baseOrigin:       originOf(baseURL),
		automationOrigin: originOf(automationBase),
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		configuredCloudID: config.CloudID,
	}, nil
}

// originOf returns the scheme://host portion of rawURL (e.g.
// "https://mysite.atlassian.net"), or "" if rawURL cannot be parsed or has
// no host.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// BaseURL returns the base URL of the Atlassian instance.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// tenantInfoResponse is the shape of the GET /_edge/tenant_info response.
type tenantInfoResponse struct {
	CloudID string `json:"cloudId"`
}

// CloudID returns the Atlassian cloud ID for the site configured on this
// client. If ClientConfig.CloudID was set, it is returned directly and no
// network call is made. Otherwise the ID is looked up via
// GET {baseURL}/_edge/tenant_info. Only a *successful* lookup is cached —
// if the lookup fails, nothing is stored and the next call to CloudID
// retries it from scratch, rather than returning the same error forever.
func (c *Client) CloudID(ctx context.Context) (string, error) {
	if c.configuredCloudID != "" {
		return c.configuredCloudID, nil
	}

	c.cloudIDMu.Lock()
	defer c.cloudIDMu.Unlock()

	if c.cloudIDValue != "" {
		return c.cloudIDValue, nil
	}

	var info tenantInfoResponse
	if err := c.Get(ctx, tenantInfoPath, &info); err != nil {
		return "", fmt.Errorf("looking up cloud ID from %s: %w", tenantInfoPath, err)
	}
	if info.CloudID == "" {
		return "", fmt.Errorf("looking up cloud ID from %s: response had no cloudId", tenantInfoPath)
	}

	c.cloudIDValue = info.CloudID
	return c.cloudIDValue, nil
}

// AutomationURL builds an absolute URL under the Atlassian Automation API
// for the given rest path (e.g. "/rule"), resolving this client's cloud ID
// first. The result is <AutomationBase>/<cloudId>/rest/v1<path> and is meant
// to be passed straight to Client.Do (or Get/Post/...), which sends
// requests to an absolute URL as-is rather than prefixing it with baseURL.
func (c *Client) AutomationURL(ctx context.Context, path string) (string, error) {
	cloudID, err := c.CloudID(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/rest/v1%s", c.automationBase, cloudID, path), nil
}

// QueryEscape escapes a string for use in URL query parameters.
func QueryEscape(s string) string {
	return url.QueryEscape(s)
}

// PathEscape escapes a string for use in URL path segments.
// Unlike QueryEscape, this encodes spaces as %20 instead of +.
func PathEscape(s string) string {
	return url.PathEscape(s)
}

// newRequest creates a new HTTP request with authentication and standard
// headers. If path is already an absolute URL (http:// or https://), it is
// used as-is instead of being appended to baseURL — this lets callers reach
// hosts other than the configured Atlassian site (e.g. the Automation API).
// An absolute URL is only allowed when its scheme+host matches the
// configured site (baseURL) or the configured AutomationBase; anything else
// is rejected here, before Basic auth is attached or any network call is
// made.
func (c *Client) newRequest(ctx context.Context, method, path string, body *bytes.Reader) (*http.Request, error) {
	u := path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		origin := originOf(path)
		if origin == "" || (origin != c.baseOrigin && origin != c.automationOrigin) {
			return nil, fmt.Errorf("refusing to send request to disallowed absolute URL %q: host must match the configured site (%s) or Automation API (%s)", path, c.baseOrigin, c.automationOrigin)
		}
	} else {
		u = c.baseURL + path
	}

	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, u, body)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(c.user, c.token)
	req.Header.Set("User-Agent", fmt.Sprintf("terraform-provider-atlassian/%s", c.version))
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// Do executes an HTTP request with retry logic for rate limits and server errors.
// The body parameter uses bytes.NewReader so it can be replayed on retries.
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Reset body reader position for retries
		if bodyReader != nil {
			_, _ = bodyReader.Seek(0, io.SeekStart)
		}

		req, err := c.newRequest(ctx, method, path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("executing request: %w", err)
		}

		// Rate limited
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			if attempt == maxRetries {
				return nil, fmt.Errorf("rate limited after %d retries", maxRetries)
			}
			delay := parseRetryAfter(resp.Header.Get("Retry-After"))
			if err := sleepWithContext(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}

		// Server error — retry with exponential backoff
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			if attempt == maxRetries {
				return nil, fmt.Errorf("server error (HTTP %d) after %d retries", resp.StatusCode, maxRetries)
			}
			delay := exponentialBackoff(attempt)
			if err := sleepWithContext(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("request failed after %d retries", maxRetries)
}

// Get performs a GET request and decodes the JSON response into v.
func (c *Client) Get(ctx context.Context, path string, v interface{}) error {
	resp, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	return json.NewDecoder(resp.Body).Decode(v)
}

// GetWithStatus performs a GET request and returns the status code.
// On 404, returns (404, nil) — the caller decides whether to remove state.
func (c *Client) GetWithStatus(ctx context.Context, path string, v interface{}) (int, error) {
	resp, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return http.StatusNotFound, nil
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("GET %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return resp.StatusCode, err
	}

	return resp.StatusCode, nil
}

// Post performs a POST request with a JSON body and decodes the response into v.
func (c *Client) Post(ctx context.Context, path string, body interface{}, v interface{}) error {
	_, err := c.PostWithStatus(ctx, path, body, v)
	return err
}

// PostWithStatus is like Post but also returns the HTTP status code, so a
// caller can react to a specific non-2xx code (e.g. 409 while another
// workflow configuration task is running) instead of only seeing an error.
// On a non-2xx status the returned error carries the response body.
func (c *Client) PostWithStatus(ctx context.Context, path string, body interface{}, v interface{}) (int, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshaling request body: %w", err)
	}

	resp, err := c.Do(ctx, "POST", path, jsonBody)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("POST %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	if v != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(v)
	}

	return resp.StatusCode, nil
}

// Put performs a PUT request with a JSON body and decodes the response into v.
func (c *Client) Put(ctx context.Context, path string, body interface{}, v interface{}) error {
	_, err := c.PutWithStatus(ctx, path, body, v)
	return err
}

// PutWithStatus is like Put but also returns the HTTP status code (see
// PostWithStatus). On a non-2xx status the returned error carries the body.
func (c *Client) PutWithStatus(ctx context.Context, path string, body interface{}, v interface{}) (int, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshaling request body: %w", err)
	}

	resp, err := c.Do(ctx, "PUT", path, jsonBody)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("PUT %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	if v != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(v)
	}

	return resp.StatusCode, nil
}

// Patch performs a PATCH request with a JSON body and decodes the response into v.
func (c *Client) Patch(ctx context.Context, path string, body interface{}, v interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling request body: %w", err)
	}

	resp, err := c.Do(ctx, "PATCH", path, jsonBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PATCH %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	if v != nil {
		return json.NewDecoder(resp.Body).Decode(v)
	}

	return nil
}

// Delete performs a DELETE request.
func (c *Client) Delete(ctx context.Context, path string) error {
	resp, err := c.Do(ctx, "DELETE", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// DeleteWithStatus performs a DELETE request and returns the HTTP status code.
// On 404, returns (404, nil) — the caller decides how to handle it.
func (c *Client) DeleteWithStatus(ctx context.Context, path string) (int, error) {
	resp, err := c.Do(ctx, "DELETE", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return http.StatusNotFound, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("DELETE %s: unexpected status %d: %s", path, resp.StatusCode, string(bodyBytes))
	}

	return resp.StatusCode, nil
}

// parseRetryAfter parses the Retry-After header value as seconds.
// Returns baseDelay if the header cannot be parsed.
// Caps the value at maxRetryAfterSeconds to prevent multi-minute sleeps.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return baseDelay
	}

	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return baseDelay
	}

	if seconds > maxRetryAfterSeconds {
		seconds = maxRetryAfterSeconds
	}

	return time.Duration(seconds) * time.Second
}

// sleepWithContext sleeps for d or until ctx is cancelled, whichever comes first.
// Returns ctx.Err() if the context is cancelled before the sleep completes.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exponentialBackoff calculates the delay for a given retry attempt with jitter
// to prevent thundering herd on rate limits.
func exponentialBackoff(attempt int) time.Duration {
	delay := time.Duration(math.Pow(2, float64(attempt))) * baseDelay
	jitter := 0.5 + rand.Float64()*0.5 // 0.5 to 1.0
	delay = time.Duration(float64(delay) * 2 * jitter)
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return delay
}
