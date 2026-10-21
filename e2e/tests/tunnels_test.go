package tests

// Experiment 2 and e2e suite 4: replica identity and lifecycle, tunnel permissions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// An attacking HTTP server in the attacker container (its proxy's local target).
func attackerServer(e *h.Env, port int) {
	e.MustSh(h.Attacker, fmt.Sprintf(`nohup node -e "require('http').createServer((q,s)=>{if(q.url==='/stream'){s.write('x');return setInterval(()=>s.write('x'),200)}s.end(JSON.stringify({replica:'atk'}))}).listen(%d,'127.0.0.1')" > /tmp/atk-%d.log 2>&1 &`, port, port))
}

// 2.1: a modified frpc claims someone else's group, port or service.
func TestTunnels_2_1_ForeignGroupPortService(t *testing.T) {
	e := apiSetup(t)
	apiPort := e.Status().Status.Services[0].Port
	otherPort := e.Service("other")
	_, ca := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	_, atk := e.Token("other")
	b, _ := h.DecodeBundle(atk)
	tid := b.TokenID()
	attackerServer(e, 8081)
	proxies := fmt.Sprintf(`[
	  {"name": "api.%[1]s.x", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8081},
	  {"name": "other.%[1]s.x", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8081, "remotePort": %[2]d,
	   "loadBalancer": {"group": "svc-api", "groupKey": "zgadniety"}},
	  {"name": "other.%[1]s.web", "type": "http", "localIP": "127.0.0.1", "localPort": 8081, "customDomains": ["api.ep.test"]}
	]`, tid, apiPort)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	e.RawFrpc(h.Attacker, "atk", b, proxies, 10)
	var conf map[string]any
	e.Eventually(30*time.Second, 500*time.Millisecond, "attacker proxy in frps", func() error {
		out := e.MustSh(h.Gateway, `curl -s -u "entrypoint:$(cat /var/lib/entrypoint/secrets/frps-api)" http://127.0.0.1:7500/api/proxy/tcp/other.`+tid+`.x`)
		var p struct {
			Status string         `json:"status"`
			Conf   map[string]any `json:"conf"`
		}
		if err := json.Unmarshal([]byte(out), &p); err != nil || p.Status != "online" {
			return fmt.Errorf("missing: %s", out)
		}
		conf = p.Conf
		return nil
	})
	time.Sleep(5 * time.Second)
	ss := load.Stop()
	proxies2 := e.FrpsProxies(h.Gateway)
	j := e.Journal(h.Gateway, "entrypointd")
	toAtk := h.Count(ss, func(s h.Sample) bool { return s.Replica == "atk" })
	bad := h.Count(ss, func(s h.Sample) bool { return s.Status != 200 })
	lb, _ := conf["loadBalancer"].(map[string]any)
	result(t, "proxy with a foreign name: %q; proxy with a foreign group and port: group=%v port=%v (port other=%d, api=%d); http proxy: %q; requests to the attacker: %d; api errors: %d",
		proxies2["api."+tid+".x"], lb["group"], conf["remotePort"], otherPort, apiPort, proxies2["other."+tid+".web"], toAtk, bad)
	if _, ok := proxies2["api."+tid+".x"]; ok {
		t.Fatal("frps accepted a proxy named after another service")
	}
	if lb["group"] != "svc-other" || int(conf["remotePort"].(float64)) != otherPort {
		t.Fatalf("the plugin did not override group/port: %v", conf)
	}
	if toAtk > 0 || bad > 0 {
		t.Fatalf("impact on the api service: %d to the attacker, %d errors", toAtk, bad)
	}
	if !strings.Contains(j, "proxy registration rejected") || !strings.Contains(j, "api."+tid+".x") {
		t.Fatal("the rejection was not logged")
	}
	if got := e.ReplicaSuffixes("api"); strings.Join(got, ",") != "a" {
		t.Fatalf("api replicas: %v", got)
	}
}

func pluginCall(e *h.Env, op string, content map[string]any) map[string]any {
	e.T.Helper()
	body, _ := json.Marshal(map[string]any{"version": "0.1.0", "op": op, "content": content})
	e.WriteFile(h.Gateway, "/tmp/plugin-req.json", string(body), "600")
	out := e.MustSh(h.Gateway, `curl -s -X POST -H 'Content-Type: application/json' --data @/tmp/plugin-req.json http://127.0.0.1:7400/frps/handler?op=`+op+`; rm -f /tmp/plugin-req.json`)
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		e.T.Fatalf("plugin response: %v: %s", err, out)
	}
	return r
}

// 2.2: NewProxy fields outside the allowlist, disallowed types, a token without scope.
func TestTunnels_2_2_NewProxyWhitelist(t *testing.T) {
	e := apiSetup(t)
	apiPort := e.Status().Status.Services[0].Port
	e.Service("other")
	_, ca := e.Token("api")
	b, _ := h.DecodeBundle(ca)
	tid := b.TokenID()
	user := map[string]any{"user": "", "run_id": "r1", "metas": map[string]string{"token": b.Token}}
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"user": user, "proxy_name": "api." + tid + ".x", "proxy_type": "tcp"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name   string
		c      map[string]any
		reject bool
	}{
		{"custom_domains", base(map[string]any{"custom_domains": []string{"evil.ep.test"}}), true},
		{"subdomain", base(map[string]any{"subdomain": "evil"}), true},
		{"locations", base(map[string]any{"locations": []string{"/"}}), true},
		{"http_user", base(map[string]any{"http_user": "u"}), true},
		{"sk (stcp)", base(map[string]any{"sk": "x"}), true},
		{"multiplexer", base(map[string]any{"multiplexer": "httpconnect"}), true},
		{"http type", base(map[string]any{"proxy_type": "http"}), true},
		{"udp type", base(map[string]any{"proxy_type": "udp"}), true},
		{"stcp type", base(map[string]any{"proxy_type": "stcp"}), true},
		{"another service's name", base(map[string]any{"proxy_name": "other." + tid + ".x"}), true},
		{"name without a replica", base(map[string]any{"proxy_name": "api"}), true},
		{"group/key/port overridden", base(map[string]any{"group": "svc-other", "group_key": "x", "remote_port": 1}), false},
	}
	var lines []string
	for _, tc := range cases {
		r := pluginCall(e, "NewProxy", tc.c)
		rej, _ := r["reject"].(bool)
		lines = append(lines, fmt.Sprintf("%s: rejected=%v", tc.name, rej))
		if rej != tc.reject {
			t.Errorf("%s: reject=%v, expected %v (%v)", tc.name, rej, tc.reject, r)
		}
		if !tc.reject {
			c := r["content"].(map[string]any)
			if c["group"] != "svc-api" || int(c["remote_port"].(float64)) != apiPort || c["group_key"] == "x" {
				t.Errorf("not overridden: %v", c)
			}
		}
	}
	// A token without scope: a service removed together with its token; an invalid token; the user field.
	e.Service("tmp")
	_, ct := e.Token("tmp")
	bt, _ := h.DecodeBundle(ct)
	e.MustEP("service", "rm", "tmp")
	login := func(tok, usr string) map[string]any {
		return pluginCall(e, "Login", map[string]any{"user": usr, "metas": map[string]string{"token": tok}, "client_address": "172.28.0.99:1234", "version": "0.71.1"})
	}
	for _, c := range []struct {
		name, tok, user string
		reject          bool
	}{
		{"token of a removed service", bt.Token, "", true},
		{"mangled token", "ept_000000000000_xyz", "", true},
		{"valid token with the user field", b.Token, "admin", true},
		{"valid token", b.Token, "", false},
	} {
		r := login(c.tok, c.user)
		rej, _ := r["reject"].(bool)
		lines = append(lines, fmt.Sprintf("Login %s: rejected=%v %v", c.name, rej, r["reject_reason"]))
		if rej != c.reject {
			t.Errorf("Login %s: reject=%v", c.name, rej)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// 2.3: a removed service's port never goes to a new one.
func TestTunnels_2_3_PortsNeverReused(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	p1 := e.Service("x")
	_, oldTok := e.Token("x")
	e.MustEP("service", "rm", "x")
	p2 := e.Service("y")
	p3 := e.Service("x")
	reserved := e.MustExec(h.Gateway, "sqlite3", "/var/lib/entrypoint/db/entrypoint.db", "SELECT port || ':' || service_name FROM ports ORDER BY port")
	ob, _ := h.DecodeBundle(oldTok)
	r := pluginCall(e, "Login", map[string]any{"metas": map[string]string{"token": ob.Token}, "client_address": "172.28.0.99:1"})
	result(t, "ports: x=%d, after removal y=%d, new x=%d; reservations: %s; old token x rejected after recreating the service=%v",
		p1, p2, p3, strings.ReplaceAll(strings.TrimSpace(reserved), "\n", ", "), r["reject"])
	if p2 == p1 || p3 == p1 || p3 == p2 {
		t.Fatal("a port was reused")
	}
	if r["reject"] != true {
		t.Fatal("a removed service's token works for a new service with the same name")
	}
	if _, err := e.Exec(h.Gateway, "sqlite3", "/var/lib/entrypoint/db/entrypoint.db", "DELETE FROM ports"); err == nil {
		t.Fatal("port reservations could be deleted")
	}
}

// 2.4: entrypointd and frps restarts in different orders; status matches frps state.
func TestTunnels_2_4_StatusConsistency(t *testing.T) {
	e := apiSetup(t)
	twoReplicas(t, e, "adapter", 30000)
	steps := []struct {
		name string
		cmds []string
	}{
		{"restart frps", []string{"systemctl restart entrypoint-frps"}},
		{"restart entrypointd", []string{"systemctl restart entrypointd"}},
		{"restart frps, then entrypointd", []string{"systemctl restart entrypoint-frps", "systemctl restart entrypointd"}},
		{"restart entrypointd, then frps", []string{"systemctl restart entrypointd", "systemctl restart entrypoint-frps"}},
		{"kill -9 both at once", []string{"systemctl kill -s KILL entrypointd entrypoint-frps"}},
	}
	var lines []string
	for _, st := range steps {
		// Steps follow each other within seconds; keep systemd's start limit (5 in 10 s) out of the way.
		e.MustSh(h.Gateway, "systemctl reset-failed entrypointd entrypoint-frps")
		for _, c := range st.cmds {
			e.MustSh(h.Gateway, c)
		}
		t0 := time.Now()
		e.WaitReplicas("api", []string{"a", "b"}, 60*time.Second)
		// Comparison: entrypointd vs frps directly.
		var frpsOnline []string
		for n, s := range e.FrpsProxies(h.Gateway) {
			if s == "online" {
				frpsOnline = append(frpsOnline, n)
			}
		}
		ep := e.Replicas("api")
		same := len(ep) == len(frpsOnline)
		for _, n := range ep {
			found := false
			for _, f := range frpsOnline {
				found = found || f == n
			}
			same = same && found
		}
		lines = append(lines, fmt.Sprintf("%s: consistent=%v after %s", st.name, same, ms(time.Since(t0))))
		if !same {
			t.Errorf("%s: entrypointd %v, frps %v", st.name, ep, frpsOnline)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// 2.5: CA rotation.
func TestTunnels_2_5_CARotation(t *testing.T) {
	e := apiSetup(t)
	_, oldBundle := e.Token("api")
	x := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: oldBundle, Replica: "x", Wait: true})
	e.MustEP("ca", "rotate", "start")
	_, both := e.Token("api")
	bb, _ := h.DecodeBundle(both)
	y := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: both, Replica: "y", Wait: true})
	var st struct {
		State        string
		StaleClients []struct{ ID string }
	}
	e.EPJSON(&st, "ca", "status")
	ob, _ := h.DecodeBundle(oldBundle)
	staleBefore := len(st.StaleClients) == 1 && st.StaleClients[0].ID == ob.TokenID()
	e.MustEP("ca", "rotate", "switch")
	t0 := time.Now()
	// frps restarts with the new CA's certificate (.path unit); y comes back, x is rejected.
	e.Eventually(60*time.Second, 500*time.Millisecond, "y connected after the switch", func() error {
		s := y.MustStats()
		if s.EventTime("disconnected") == 0 || s.EventTime("healthy") < s.EventTime("disconnected") {
			return errors.New("y not back yet")
		}
		return nil
	})
	yBack := time.Since(t0)
	e.Eventually(20*time.Second, 300*time.Millisecond, "tls_error of the client with the old CA", func() error {
		if !x.MustStats().HasEvent("tls_error") {
			return errors.New("missing")
		}
		return nil
	})
	xs := x.MustStats()
	xBack := xs.EventTime("healthy") > xs.EventTime("disconnected") && xs.EventTime("disconnected") > 0
	reps := e.ReplicaSuffixes("api")
	e.MustEP("ca", "rotate", "finish")
	_, newOnly := e.Token("api")
	nb, _ := h.DecodeBundle(newOnly)
	result(t, "bundle at start: %d CAs; ca status before the switch shows the client with the old bundle: %v; after the switch y came back after %s, x (old CA only) reconnected: %v, tls_error=%v; replicas: %v; bundle after finish: %d CAs",
		len(bb.CAs), staleBefore, ms(yBack), xBack, xs.HasEvent("tls_error"), reps, len(nb.CAs))
	if len(bb.CAs) != 2 || !staleBefore || xBack || strings.Join(reps, ",") != "y" || len(nb.CAs) != 1 {
		t.Fatal("CA rotation does not meet the criterion")
	}
}

// Issue #2: a valid token holder doing rapid login/registration cycles is throttled by the
// gateway, and another service stays healthy. Thresholds fixed before measuring:
// B has 0 errors and p95 latency within baseline p95 + 200 ms.
func TestTunnels_TokenAbuseIsolation(t *testing.T) {
	e, _, _, _ := revokeSetup(t, 10)
	load := e.StartLoad("https://b.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(5 * time.Second)
	baseline := h.P95(load.Snapshot())
	t0 := time.Now()
	// 45 login+register cycles in ~15 s with the same valid token (modified client, no connector).
	e.KillRawFrpc(h.Attacker, "a")
	e.MustSh(h.Attacker, `for i in $(seq 1 45); do EP_TOK="$(cat /run/ep/a.tok)" timeout 0.3 frpc -c /run/ep/a.json >> /tmp/frpc-abuse.log 2>&1; done; true`)
	abuse := time.Since(t0)
	time.Sleep(2 * time.Second)
	ss := load.Stop()
	win := h.Window(ss, t0, time.Now())
	bad := h.Count(win, func(s h.Sample) bool { return s.Status != 200 })
	during := h.P95(win)
	logs, _ := e.Sh(h.Attacker, "cat /tmp/frpc-abuse.log")
	limited := strings.Count(logs, "ep:rate_limited")
	result(t, "45 login cycles of token A in %s: rate-limited %d times; service B: %d errors of %d, p95 %s (baseline %s)",
		ms(abuse), limited, bad, len(win), ms(during), ms(baseline))
	if limited == 0 {
		t.Fatal("rapid logins of a valid token were not limited")
	}
	if bad > 0 || during > baseline+200*time.Millisecond {
		t.Fatal("service B degraded by token A")
	}
}
