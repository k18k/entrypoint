// Package caddyctl talks to the Caddy admin API over a local unix socket.
package caddyctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	http *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://caddy"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Caddy admin API unavailable: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Caddy rejected %s %s (status %d): %s", method, path, resp.StatusCode, bytes.TrimSpace(out))
	}
	return out, nil
}

// Load applies a full configuration. Caddy applies it atomically or keeps the previous one.
func (c *Client) Load(ctx context.Context, cfg []byte) error {
	_, err := c.do(ctx, "POST", "/load", cfg)
	return err
}

// Config returns Caddy's current configuration.
func (c *Client) Config(ctx context.Context) ([]byte, error) {
	return c.do(ctx, "GET", "/config/", nil)
}
