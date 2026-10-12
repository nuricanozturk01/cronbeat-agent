package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	connectTimeout = 5 * time.Second
	totalTimeout   = 15 * time.Second
	maxBodyBytes   = 1 << 20
)

// RetryConfig controls the retry loop of a single API call.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts (first try included).
	MaxAttempts int
	// BaseDelay is the backoff before the second attempt; it doubles per attempt.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
	// MaxRetryAfter caps a server supplied Retry-After.
	MaxRetryAfter time.Duration
}

// DefaultRetry is the production retry policy.
func DefaultRetry() RetryConfig {
	return RetryConfig{
		MaxAttempts:   5,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      30 * time.Second,
		MaxRetryAfter: 2 * time.Minute,
	}
}

// StatusError is returned for a non-2xx response.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("cronbeat api returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("cronbeat api returned HTTP %d: %s", e.Status, e.Body)
}

// IsUnauthorized reports whether err is a 401 response (invalid API key).
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusUnauthorized
}

// IsPermanent reports whether retrying the same request can never help
// (any response other than 401, 429 and 5xx).
func IsPermanent(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return !retryableStatus(se.Status) && se.Status != http.StatusUnauthorized
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// Client talks to the CronBeat agent API. The API key is only ever placed in
// the Authorization header; it is never logged or included in errors.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
	log     *slog.Logger
	retry   RetryConfig
	sleep   func(ctx context.Context, d time.Duration) error
	jitter  func() float64
	agent   string
}

// Option customises a Client.
type Option func(*Client)

// WithUserAgent sets the User-Agent header, so the API operator can tell agent versions apart.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.agent = ua }
}

// NewHTTPClient returns an http.Client with the agent's timeouts
// (connect 5s, total 15s) and a small idle pool. Redirects are not followed.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = connectTimeout
	// One agent talks to one host: a few idle connections are plenty and the
	// default pool of 100 would only hold memory.
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 60 * time.Second
	return &http.Client{
		Timeout:   totalTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewClient builds a Client. httpClient may be nil (NewHTTPClient is used).
func NewClient(baseURL, apiKey string, httpClient *http.Client, retry RetryConfig, log *slog.Logger, opts ...Option) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	if log == nil {
		log = slog.Default()
	}
	if retry.MaxAttempts < 1 {
		retry.MaxAttempts = 1
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     apiKey,
		http:    httpClient,
		log:     log,
		retry:   retry,
		sleep:   sleepCtx,
		jitter:  rand.Float64,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Sync posts the full CronJob list.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (SyncResponse, error) {
	if req.CronJobs == nil {
		req.CronJobs = []CronJob{}
	}
	var resp SyncResponse
	err := c.do(ctx, "/agent/v1/sync", req, &resp)
	return resp, err
}

// SendEvent posts one job event.
func (c *Client) SendEvent(ctx context.Context, ev Event) error {
	return c.do(ctx, "/agent/v1/events", ev, nil)
}

// Heartbeat posts a liveness heartbeat.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) error {
	return c.do(ctx, "/agent/v1/heartbeat", req, nil)
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		retryAfter, err := c.once(ctx, path, body, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var se *StatusError
		isStatus := errors.As(err, &se)
		if isStatus && !retryableStatus(se.Status) {
			if se.Status == http.StatusUnauthorized {
				c.log.Error("cronbeat api rejected the API key (HTTP 401); check CRONBEAT_API_KEY",
					"path", path)
			}
			return err
		}
		if attempt >= c.retry.MaxAttempts {
			return err
		}
		delay := c.backoff(attempt, retryAfter)
		c.log.Warn("cronbeat api call failed, retrying",
			"path", path, "attempt", attempt, "delay", delay.String(), "error", err.Error())
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, c.retry.MaxRetryAfter)
	}
	exp := c.retry.BaseDelay << (attempt - 1)
	if exp <= 0 || exp > c.retry.MaxDelay {
		exp = c.retry.MaxDelay
	}
	// equal jitter: half fixed, half random
	return exp/2 + time.Duration(c.jitter()*float64(exp/2))
}

func (c *Client) once(ctx context.Context, path string, body []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.agent != "" {
		req.Header.Set("User-Agent", c.agent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return 0, fmt.Errorf("read response of %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return parseRetryAfter(resp.Header.Get("Retry-After")), &StatusError{Status: resp.StatusCode, Body: snippet}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, fmt.Errorf("decode response of %s: %w", path, err)
		}
	}
	return 0, nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
