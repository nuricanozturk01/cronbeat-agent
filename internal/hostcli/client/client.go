package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"cronbeat.dev/cronbeat-agent/internal/hostcli/config"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/mask"
	"cronbeat.dev/cronbeat-agent/internal/hostcli/tail"
	"cronbeat.dev/cronbeat-agent/internal/hostversion"
)

const (
	// ConnectTimeout bounds dialing and the TLS handshake of the default HTTP client.
	ConnectTimeout = 2 * time.Second

	maxResponseBytes = 64 * 1024
	maxDetailLength  = 200

	maxRetryAfterSeconds = 3600
	runsPath             = "/agent/v1/host/runs"
	checkPath            = "/agent/v1/host/check"
)

// Policy bounds the time and the attempts of one API call.
type Policy struct {
	AttemptTimeout time.Duration
	MaxAttempts    int
	// Backoff[i] is the wait before attempt i+2; the last entry repeats.
	Backoff       []time.Duration
	RetryAfterCap time.Duration
	TotalBudget   time.Duration
}

// StartedPolicy never delays the job: one attempt, 4 seconds.
var StartedPolicy = Policy{AttemptTimeout: 4 * time.Second, MaxAttempts: 1, TotalBudget: 4 * time.Second}

// FinishPolicy retries up to 3 times within 12 seconds.
var FinishPolicy = Policy{
	AttemptTimeout: 4 * time.Second,
	MaxAttempts:    3,
	Backoff:        []time.Duration{300 * time.Millisecond, 900 * time.Millisecond},
	RetryAfterCap:  3 * time.Second,
	TotalBudget:    12 * time.Second,
}

// CheckPolicy is for the interactive check command: one patient attempt.
var CheckPolicy = Policy{AttemptTimeout: 10 * time.Second, MaxAttempts: 1, TotalBudget: 10 * time.Second}

// StatusError is returned for a non-2xx response.
type StatusError struct {
	Status int
	Detail string
}

func (e *StatusError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("CronBeat API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("CronBeat API returned HTTP %d: %s", e.Status, e.Detail)
}

// Kind classifies an error for rate limited warnings.
func Kind(err error) string {
	if err == nil {
		return ""
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return "network"
	}
	switch {
	case se.Status == http.StatusUnauthorized:
		return "auth"
	case se.Status == http.StatusForbidden:
		return "forbidden"
	case se.Status == http.StatusTooManyRequests:
		return "rate-limited"
	case se.Status == http.StatusBadRequest, se.Status == http.StatusRequestEntityTooLarge, se.Status == http.StatusUnprocessableEntity:
		return "bad-request"
	case se.Status >= 500:
		return "server"
	default:
		return "other"
	}
}

// Client posts job events.
type Client struct {
	baseURL string
	key     config.Secret
	http    *http.Client
	sleep   func(context.Context, time.Duration) error
	now     func() time.Time
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New returns a Client for baseURL.
func New(baseURL string, key config.Secret, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http:    newHTTPClient(),
		sleep:   sleep,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Format never prints the key.
func (c *Client) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "client.Client{baseURL:%s key:%s}", c.baseURL, c.key)
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: ConnectTimeout}).DialContext
	transport.TLSHandshakeTimeout = ConnectTimeout
	transport.DisableKeepAlives = true
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PostRun sends one run event.
func (c *Client) PostRun(ctx context.Context, req Request, p Policy) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("encode request: %w", err)
	}
	var resp Response
	err = c.post(ctx, runsPath, body, p, &resp)
	return resp, err
}

// Check validates the API key.
func (c *Client) Check(ctx context.Context, p Policy) (CheckResponse, error) {
	var resp CheckResponse
	err := c.post(ctx, checkPath, []byte("{}"), p, &resp)
	return resp, err
}

func (c *Client) post(ctx context.Context, path string, body []byte, p Policy, out any) error {
	begin := c.now()
	for attempt := 1; ; attempt++ {
		timeout := p.AttemptTimeout
		if remaining := p.TotalBudget - c.now().Sub(begin); remaining < timeout {
			timeout = remaining
		}
		if timeout <= 0 {
			return errors.New("time budget exhausted")
		}
		retryAfter, err := c.once(ctx, path, body, timeout, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !retryable(err) || attempt >= p.MaxAttempts {
			return err
		}
		delay := backoff(p, attempt, retryAfter)
		if c.now().Sub(begin)+delay >= p.TotalBudget {
			return err
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func backoff(p Policy, attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, p.RetryAfterCap)
	}
	if len(p.Backoff) == 0 {
		return 0
	}
	return p.Backoff[min(attempt-1, len(p.Backoff)-1)]
}

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return true
}

func (c *Client) once(ctx context.Context, path string, body []byte, timeout time.Duration, out any) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("cannot build the request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cronbeat-host/"+hostversion.Version)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, c.networkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
			&StatusError{Status: resp.StatusCode, Detail: c.detail(data)}
	}
	if readErr == nil && out != nil && len(bytes.TrimSpace(data)) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return 0, nil
}

func (c *Client) networkError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return errors.New(c.clean(err.Error()))
}

func (c *Client) detail(body []byte) string {
	var problem struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	text := string(body)
	if json.Unmarshal(body, &problem) == nil {
		text = problem.Detail
		if text == "" {
			text = problem.Title
		}
	}
	return c.clean(text)
}

func (c *Client) clean(text string) string {
	text = strings.ReplaceAll(text, c.key.Reveal(), "***")
	text = mask.Text(text)
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	text = strings.Join(strings.Fields(b.String()), " ")
	if len(text) > maxDetailLength {
		text = tail.Last(text, maxDetailLength)
	}
	return text
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}
