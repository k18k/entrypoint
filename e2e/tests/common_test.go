package tests

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// apiSetup: a clean gateway, an api service with a health check, an api.ep.test route.
func apiSetup(t *testing.T, extra ...string) *h.Env {
	e := h.New(t)
	e.StopAllApps()
	e.FreshGateway(extra...)
	e.SetDNS(map[string]string{"api.ep.test": h.GatewayIP})
	e.Service("api", "--health", "/health")
	e.Route("api.ep.test", "--service", "api")
	t.Cleanup(e.StopAllApps)
	return e
}

func result(t *testing.T, f string, a ...any) {
	t.Helper()
	t.Logf("RESULT: "+f, a...)
}

func get(c *http.Client, url string) (int, string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func replicaOf(body string) string {
	var v struct{ Replica string }
	_ = json.Unmarshal([]byte(body), &v)
	return v.Replica
}

// slowReader yields n bytes of random data at a limited rate.
type slowReader struct {
	left, rate, sent int
	start            time.Time
	h                io.Writer
}

func newSlowReader(n, bytesPerSec int) (*slowReader, func() string) {
	hs := sha256.New()
	return &slowReader{left: n, rate: bytesPerSec, h: hs}, func() string { return hex.EncodeToString(hs.Sum(nil)) }
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if r.start.IsZero() {
		r.start = time.Now()
	}
	// Wait until the average rate drops to the target.
	due := r.start.Add(time.Duration(float64(r.sent) / float64(r.rate) * float64(time.Second)))
	if d := time.Until(due); d > 0 {
		time.Sleep(d)
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	if n > 64<<10 {
		n = 64 << 10
	}
	_, _ = rand.Read(p[:n])
	r.h.Write(p[:n])
	r.left -= n
	r.sent += n
	return n, nil
}

type uploadResult struct {
	status int
	body   string
	err    error
	want   string
	took   time.Duration
}

func upload(c *http.Client, url string, n, rate int) <-chan uploadResult {
	ch := make(chan uploadResult, 1)
	go func() {
		start := time.Now()
		r, sum := newSlowReader(n, rate)
		c2 := *c
		c2.Timeout = 5 * time.Minute
		c = &c2
		req, _ := http.NewRequest("POST", url, r)
		req.ContentLength = int64(n)
		resp, err := c.Do(req)
		res := uploadResult{err: err, took: time.Since(start)}
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			res.status, res.body = resp.StatusCode, string(b)
		}
		res.want = sum()
		ch <- res
	}()
	return ch
}

func mustJSON(v any) string {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(v)
	return strings.TrimSpace(b.String())
}

func ms(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }

func httpNewRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, "GET", url, nil)
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
