package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Sample to wynik jednego żądania generatora ruchu.
type Sample struct {
	At      time.Time
	Status  int // 0 = błąd transportu
	Replica string
	Err     string
	Dur     time.Duration
}

// Load wysyła ciągły ruch GET do url z kilku gorutyn (każda z własnym keep-alive).
type Load struct {
	mu      sync.Mutex
	Samples []Sample
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (e *Env) StartLoad(url string, workers int, every time.Duration) *Load {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Load{cancel: cancel}
	for i := 0; i < workers; i++ {
		c := e.HTTPClient()
		c.Timeout = 10 * time.Second
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			for ctx.Err() == nil {
				s := Sample{At: time.Now()}
				req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
				resp, err := c.Do(req)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					s.Err = err.Error()
				} else {
					s.Status = resp.StatusCode
					var body struct{ Replica string }
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					_ = json.Unmarshal(b, &body)
					s.Replica = body.Replica
				}
				s.Dur = time.Since(s.At)
				l.mu.Lock()
				l.Samples = append(l.Samples, s)
				l.mu.Unlock()
				time.Sleep(every)
			}
		}()
	}
	return l
}

func (l *Load) Stop() []Sample {
	l.cancel()
	l.wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Sample(nil), l.Samples...)
}

func (l *Load) Snapshot() []Sample {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Sample(nil), l.Samples...)
}

// Window zwraca próbki z przedziału [from, to).
func Window(ss []Sample, from, to time.Time) []Sample {
	var out []Sample
	for _, s := range ss {
		if !s.At.Before(from) && s.At.Before(to) {
			out = append(out, s)
		}
	}
	return out
}

// Count liczy próbki spełniające warunek.
func Count(ss []Sample, f func(Sample) bool) int {
	n := 0
	for _, s := range ss {
		if f(s) {
			n++
		}
	}
	return n
}

// P95 returns the 95th percentile request duration.
func P95(ss []Sample) time.Duration {
	if len(ss) == 0 {
		return 0
	}
	d := make([]time.Duration, len(ss))
	for i, s := range ss {
		d[i] = s.Dur
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)*95/100]
}
