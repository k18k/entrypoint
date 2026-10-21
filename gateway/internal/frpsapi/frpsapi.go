// Package frpsapi reads proxy and client state from the local frps API (127.0.0.1).
package frpsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	Addr         string
	User         string
	PasswordFile string
	HTTP         *http.Client
}

func New(addr, passwordFile string) *Client {
	return &Client{Addr: addr, User: "entrypoint", PasswordFile: passwordFile, HTTP: &http.Client{Timeout: 3 * time.Second}}
}

type Proxy struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	CurConns int64  `json:"curConns"`
	ClientID string `json:"clientID"`
	Conf     struct {
		RemotePort   int `json:"remotePort"`
		LoadBalancer struct {
			Group string `json:"group"`
		} `json:"loadBalancer"`
	} `json:"conf"`
	LastStartTime string `json:"lastStartTime"`
	LastCloseTime string `json:"lastCloseTime"`
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	pw, err := os.ReadFile(c.PasswordFile)
	if err != nil {
		return fmt.Errorf("frps API password: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+c.Addr+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, strings.TrimSpace(string(pw)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("frps API unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("frps API: status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// TCPProxies returns the tcp proxies frps knows about (including offline ones).
func (c *Client) TCPProxies(ctx context.Context) ([]Proxy, error) {
	var out struct {
		Proxies []Proxy `json:"proxies"`
	}
	if err := c.get(ctx, "/api/proxy/tcp", &out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

// Online returns only active proxies.
func (c *Client) Online(ctx context.Context) ([]Proxy, error) {
	ps, err := c.TCPProxies(ctx)
	if err != nil {
		return nil, err
	}
	out := ps[:0]
	for _, p := range ps {
		if p.Status == "online" {
			out = append(out, p)
		}
	}
	return out, nil
}

// ServerInfo checks that frps responds.
func (c *Client) ServerInfo(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.get(ctx, "/api/serverinfo", &out)
	return out, err
}
