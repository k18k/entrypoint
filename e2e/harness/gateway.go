package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status to fragment `entrypoint status --json`.
type Status struct {
	Units  map[string]string `json:"units"`
	Error  string            `json:"error"`
	Status struct {
		Services []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Replicas []struct {
				Name     string `json:"name"`
				TokenID  string `json:"tokenId"`
				CurConns int64  `json:"curConns"`
			} `json:"replicas"`
		} `json:"services"`
		AuthzRevision int64    `json:"authzRevision"`
		FrpsOK        bool     `json:"frpsOk"`
		CaddyOK       bool     `json:"caddyOk"`
		Warnings      []string `json:"warnings"`
		LastApplied   *struct {
			Num int64 `json:"num"`
		} `json:"lastApplied"`
	} `json:"status"`
}

func (e *Env) Status() Status {
	e.T.Helper()
	var s Status
	out, _ := e.EP("status", "--json")
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		e.T.Fatalf("status: %v: %s", err, out)
	}
	return s
}

// Replicas zwraca posortowane nazwy replik serwisu według entrypointd.
func (e *Env) Replicas(service string) []string {
	e.T.Helper()
	var out []string
	for _, s := range e.Status().Status.Services {
		if s.Name == service {
			for _, r := range s.Replicas {
				out = append(out, r.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ReplicaSuffixes zwraca ostatnie człony nazw replik (np. "a", "b").
func (e *Env) ReplicaSuffixes(service string) []string {
	var out []string
	for _, n := range e.Replicas(service) {
		parts := strings.Split(n, ".")
		out = append(out, parts[len(parts)-1])
	}
	sort.Strings(out)
	return out
}

func (e *Env) WaitReplicas(service string, want []string, timeout time.Duration) time.Duration {
	e.T.Helper()
	start := time.Now()
	sort.Strings(want)
	e.Eventually(timeout, 250*time.Millisecond, fmt.Sprintf("repliki %s = %v", service, want), func() error {
		got := e.ReplicaSuffixes(service)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("są %v", got)
		}
		return nil
	})
	return time.Since(start)
}

// FrpsProxies czyta stan proxy bezpośrednio z API frps (porównanie ze statusem entrypointd).
func (e *Env) FrpsProxies(container string) map[string]string {
	e.T.Helper()
	out := e.MustSh(container, `curl -s -u "entrypoint:$(cat /var/lib/entrypoint/secrets/frps-api)" http://127.0.0.1:7500/api/proxy/tcp`)
	var r struct {
		Proxies []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		e.T.Fatalf("API frps: %v: %s", err, out)
	}
	m := map[string]string{}
	for _, p := range r.Proxies {
		m[p.Name] = p.Status
	}
	return m
}

// Journal zwraca dziennik usługi bramy od początku testu.
func (e *Env) Journal(container, unit string) string {
	out, _ := e.Exec(container, "journalctl", "-u", unit, "--no-pager", "-o", "cat", "--since", "@"+fmt.Sprint(e.start.Unix()))
	return out
}
