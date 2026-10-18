// Pakiet api definiuje typy API entrypointd i klienta używanego przez CLI.
// API jest dostępne wyłącznie przez lokalny socket unixowy.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/store"
)

// ChangeResult to wynik polecenia zmieniającego konfigurację.
type ChangeResult struct {
	Revision *store.Revision `json:"revision,omitempty"`
	Applied  bool            `json:"applied"`
	Error    string          `json:"error,omitempty"`
	Service  *ServiceInfo    `json:"service,omitempty"`
	Route    *RouteInfo      `json:"route,omitempty"`
	Token    *TokenCreated   `json:"token,omitempty"`
}

type ServiceInfo struct {
	Name     string        `json:"name"`
	Port     int           `json:"port"`
	Health   string        `json:"health,omitempty"`
	Routes   []string      `json:"routes,omitempty"`
	Replicas []ReplicaInfo `json:"replicas"`
}

type ReplicaInfo struct {
	Name     string `json:"name"`
	TokenID  string `json:"tokenId"`
	Status   string `json:"status"`
	CurConns int64  `json:"curConns"`
	Since    string `json:"since,omitempty"`
}

type RouteInfo struct {
	ID                    string `json:"id"`
	Host                  string `json:"host"`
	Prefix                string `json:"prefix"`
	Service               string `json:"service,omitempty"`
	Upstream              string `json:"upstream,omitempty"`
	StripPrefix           bool   `json:"stripPrefix,omitempty"`
	MaxBodyBytes          int64  `json:"maxBodyBytes"`
	ResponseHeaderTimeout int    `json:"responseHeaderTimeout"`
	ACME                  string `json:"acme,omitempty"`
}

type TokenCreated struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Connect string `json:"connect"` // ENTRYPOINT_CONNECT, zwracany tylko raz
}

type TokenInfo struct {
	ID          string       `json:"id"`
	Service     string       `json:"service"`
	Scope       string       `json:"scope"`
	Label       string       `json:"label,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	MaxReplicas int          `json:"maxReplicas"`
	Revoked     bool         `json:"revoked"`
	RevokedAt   *time.Time   `json:"revokedAt,omitempty"`
	LastLogin   *store.Login `json:"lastLogin,omitempty"`
}

type ComponentInfo struct {
	Name    string `json:"name"`
	Applied int64  `json:"applied"`
}

type Status struct {
	Version     string          `json:"version"`
	GatewayName string          `json:"gatewayName"`
	TunnelPort  int             `json:"tunnelPort"`
	Head        *store.Revision `json:"head"`
	LastApplied *store.Revision `json:"lastApplied"`
	Components  []ComponentInfo `json:"components"`
	AuthzRev    int64           `json:"authzRevision"`
	Services    []ServiceInfo   `json:"services"`
	FrpsOK      bool            `json:"frpsOk"`
	CaddyOK     bool            `json:"caddyOk"`
	Warnings    []string        `json:"warnings"`
}

type CAInfo struct {
	State        string      `json:"state"` // "", "staged", "switched"
	Current      CertInfo    `json:"current"`
	Next         *CertInfo   `json:"next,omitempty"`
	Old          *CertInfo   `json:"old,omitempty"`
	Server       CertInfo    `json:"server"`
	BundleCAs    []string    `json:"bundleCAs"`
	StaleClients []TokenInfo `json:"staleClients"`
}

type CertInfo struct {
	Fingerprint string    `json:"fingerprint"`
	Subject     string    `json:"subject"`
	NotAfter    time.Time `json:"notAfter"`
	Issuer      string    `json:"issuer,omitempty"`
}

// Żądania.
type ServiceAdd struct {
	Name    string `json:"name"`
	Health  string `json:"health,omitempty"`
	BaseRev *int64 `json:"baseRev,omitempty"`
}

type RouteAdd struct {
	Host                  string `json:"host"`
	Prefix                string `json:"prefix"`
	Service               string `json:"service,omitempty"`
	Upstream              string `json:"upstream,omitempty"`
	StripPrefix           bool   `json:"stripPrefix,omitempty"`
	MaxBodyBytes          int64  `json:"maxBodyBytes,omitempty"`
	ResponseHeaderTimeout int    `json:"responseHeaderTimeout,omitempty"`
	ACME                  string `json:"acme,omitempty"`
	BaseRev               *int64 `json:"baseRev,omitempty"`
}

type TokenCreate struct {
	Service     string `json:"service"`
	Label       string `json:"label,omitempty"`
	MaxReplicas int    `json:"maxReplicas,omitempty"`
	BaseRev     *int64 `json:"baseRev,omitempty"`
}

type TokenRevoke struct {
	Reason string `json:"reason,omitempty"`
}

type CARotate struct {
	Step string `json:"step"` // start, switch, finish
}

// Error to błąd zwracany przez API.
type Error struct {
	Status  int           `json:"-"`
	Message string        `json:"error"`
	Result  *ChangeResult `json:"result,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Client to klient API przez socket unixowy.
type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 5 * time.Minute}}
}

func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://entrypointd"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("brak połączenia z entrypointd (czy usługa działa i czy masz uprawnienia, np. sudo?): %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(raw, e) != nil || e.Message == "" {
			e.Message = fmt.Sprintf("entrypointd: kod %d", resp.StatusCode)
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func PathEscape(s string) string { return url.PathEscape(s) }
