// Package external implements the outbound "API_EXTERNAL" callback protocol that
// emya defines in api.md. When API_EXTERNAL is set, certain events (user login,
// search, favorite, video URL, subtitle URL) are POST-ed to that endpoint.
package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Response is the envelope every external endpoint returns.
type Response struct {
	Code    int             `json:"code"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Client wraps the outbound HTTP client.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient constructs a client. If baseURL is empty the client is a no-op.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether the external API is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != ""
}

// Post issues an authenticated POST to baseURL+path with a JSON body and decodes the response.
func (c *Client) Post(ctx context.Context, path string, body any) (*Response, error) {
	if !c.Enabled() {
		return nil, errors.New("external api not configured")
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("API_KEY", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	out := &Response{}
	if len(data) == 0 {
		out.Code = resp.StatusCode
		return out, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		// Not JSON; treat as opaque message.
		out.Code = resp.StatusCode
		out.Message = string(data)
		return out, nil
	}
	return out, nil
}
