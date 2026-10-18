package tests

// Eksperyment 2 i zestaw e2e 4: tożsamość i cykl życia replik, uprawnienia tuneli.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Atakujący serwer HTTP w kontenerze attacker (cel lokalny jego proxy).
func attackerServer(e *h.Env, port int) {
	e.MustSh(h.Attacker, fmt.Sprintf(`nohup node -e "require('http').createServer((q,s)=>s.end(JSON.stringify({replica:'atk'}))).listen(%d,'127.0.0.1')" > /tmp/atk-%d.log 2>&1 &`, port, port))
}

// 2.1: zmodyfikowany frpc podaje cudzą grupę, port lub serwis.
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
	e.Eventually(30*time.Second, 500*time.Millisecond, "proxy atakującego w frps", func() error {
		out := e.MustSh(h.Gateway, `curl -s -u "entrypoint:$(cat /var/lib/entrypoint/secrets/frps-api)" http://127.0.0.1:7500/api/proxy/tcp/other.`+tid+`.x`)
		var p struct {
			Status string         `json:"status"`
			Conf   map[string]any `json:"conf"`
		}
		if err := json.Unmarshal([]byte(out), &p); err != nil || p.Status != "online" {
			return fmt.Errorf("brak: %s", out)
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
	result(t, "proxy z cudzą nazwą: %q; proxy z cudzą grupą i portem: grupa=%v port=%v (port other=%d, api=%d); proxy http: %q; żądań do atakującego: %d; błędy api: %d",
		proxies2["api."+tid+".x"], lb["group"], conf["remotePort"], otherPort, apiPort, proxies2["other."+tid+".web"], toAtk, bad)
	if _, ok := proxies2["api."+tid+".x"]; ok {
		t.Fatal("frps przyjął proxy z nazwą cudzego serwisu")
	}
	if lb["group"] != "svc-other" || int(conf["remotePort"].(float64)) != otherPort {
		t.Fatalf("plugin nie nadpisał grupy/portu: %v", conf)
	}
	if toAtk > 0 || bad > 0 {
		t.Fatalf("wpływ na serwis api: do atakującego %d, błędy %d", toAtk, bad)
	}
	if !strings.Contains(j, "rejestracja proxy odrzucona") || !strings.Contains(j, "api."+tid+".x") {
		t.Fatal("odrzucenie nie zostało zalogowane")
	}
	if got := e.ReplicaSuffixes("api"); strings.Join(got, ",") != "a" {
		t.Fatalf("repliki api: %v", got)
	}
}

func pluginCall(e *h.Env, op string, content map[string]any) map[string]any {
	e.T.Helper()
	body, _ := json.Marshal(map[string]any{"version": "0.1.0", "op": op, "content": content})
	e.WriteFile(h.Gateway, "/tmp/plugin-req.json", string(body), "600")
	out := e.MustSh(h.Gateway, `curl -s -X POST -H 'Content-Type: application/json' --data @/tmp/plugin-req.json http://127.0.0.1:7400/frps/handler?op=`+op+`; rm -f /tmp/plugin-req.json`)
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		e.T.Fatalf("odpowiedź pluginu: %v: %s", err, out)
	}
	return r
}

// 2.2: pola NewProxy spoza białej listy, niedozwolone typy, token bez zakresu.
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
		{"typ http", base(map[string]any{"proxy_type": "http"}), true},
		{"typ udp", base(map[string]any{"proxy_type": "udp"}), true},
		{"typ stcp", base(map[string]any{"proxy_type": "stcp"}), true},
		{"nazwa innego serwisu", base(map[string]any{"proxy_name": "other." + tid + ".x"}), true},
		{"nazwa bez repliki", base(map[string]any{"proxy_name": "api"}), true},
		{"grupa/klucz/port nadpisane", base(map[string]any{"group": "svc-other", "group_key": "x", "remote_port": 1}), false},
	}
	var lines []string
	for _, tc := range cases {
		r := pluginCall(e, "NewProxy", tc.c)
		rej, _ := r["reject"].(bool)
		lines = append(lines, fmt.Sprintf("%s: odrzucone=%v", tc.name, rej))
		if rej != tc.reject {
			t.Errorf("%s: reject=%v, oczekiwano %v (%v)", tc.name, rej, tc.reject, r)
		}
		if !tc.reject {
			c := r["content"].(map[string]any)
			if c["group"] != "svc-api" || int(c["remote_port"].(float64)) != apiPort || c["group_key"] == "x" {
				t.Errorf("nie nadpisano: %v", c)
			}
		}
	}
	// Token bez zakresu: serwis usunięty razem z tokenem; token niepoprawny; pole user.
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
		{"token usuniętego serwisu", bt.Token, "", true},
		{"zniekształcony token", "ept_000000000000_xyz", "", true},
		{"poprawny token z polem user", b.Token, "admin", true},
		{"poprawny token", b.Token, "", false},
	} {
		r := login(c.tok, c.user)
		rej, _ := r["reject"].(bool)
		lines = append(lines, fmt.Sprintf("Login %s: odrzucone=%v %v", c.name, rej, r["reject_reason"]))
		if rej != c.reject {
			t.Errorf("Login %s: reject=%v", c.name, rej)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// 2.3: port usuniętego serwisu nigdy nie trafia do nowego.
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
	result(t, "porty: x=%d, po usunięciu y=%d, nowy x=%d; rezerwacje: %s; stary token x po odtworzeniu serwisu odrzucony=%v",
		p1, p2, p3, strings.ReplaceAll(strings.TrimSpace(reserved), "\n", ", "), r["reject"])
	if p2 == p1 || p3 == p1 || p3 == p2 {
		t.Fatal("port został użyty ponownie")
	}
	if r["reject"] != true {
		t.Fatal("token usuniętego serwisu działa dla nowego serwisu o tej samej nazwie")
	}
	if _, err := e.Exec(h.Gateway, "sqlite3", "/var/lib/entrypoint/db/entrypoint.db", "DELETE FROM ports"); err == nil {
		t.Fatal("rezerwacje portów dały się usunąć")
	}
}

// 2.4: restarty entrypointd i frps w różnej kolejności; status zgodny ze stanem frps.
func TestTunnels_2_4_StatusConsistency(t *testing.T) {
	e := apiSetup(t)
	twoReplicas(t, e, "express", 30000)
	steps := []struct {
		name string
		cmds []string
	}{
		{"restart frps", []string{"systemctl restart entrypoint-frps"}},
		{"restart entrypointd", []string{"systemctl restart entrypointd"}},
		{"restart frps, potem entrypointd", []string{"systemctl restart entrypoint-frps", "systemctl restart entrypointd"}},
		{"restart entrypointd, potem frps", []string{"systemctl restart entrypointd", "systemctl restart entrypoint-frps"}},
		{"kill -9 obu naraz", []string{"systemctl kill -s KILL entrypointd entrypoint-frps"}},
	}
	var lines []string
	for _, st := range steps {
		for _, c := range st.cmds {
			e.MustSh(h.Gateway, c)
		}
		t0 := time.Now()
		e.WaitReplicas("api", []string{"a", "b"}, 60*time.Second)
		// Porównanie: entrypointd vs bezpośrednio frps.
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
		lines = append(lines, fmt.Sprintf("%s: zgodne=%v po %s", st.name, same, ms(time.Since(t0))))
		if !same {
			t.Errorf("%s: entrypointd %v, frps %v", st.name, ep, frpsOnline)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// 2.5: rotacja CA.
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
	// frps restartuje się z certyfikatem nowego CA (jednostka .path); y wraca, x jest odrzucany.
	e.Eventually(60*time.Second, 500*time.Millisecond, "y połączony po przełączeniu", func() error {
		s := y.MustStats()
		if s.EventTime("disconnected") == 0 || s.EventTime("healthy") < s.EventTime("disconnected") {
			return errors.New("y jeszcze nie wrócił")
		}
		return nil
	})
	yBack := time.Since(t0)
	time.Sleep(10 * time.Second)
	xs := x.MustStats()
	xBack := xs.EventTime("healthy") > xs.EventTime("disconnected") && xs.EventTime("disconnected") > 0
	reps := e.ReplicaSuffixes("api")
	e.MustEP("ca", "rotate", "finish")
	_, newOnly := e.Token("api")
	nb, _ := h.DecodeBundle(newOnly)
	result(t, "pakiet start: %d CA; ca status przed przełączeniem wskazuje klienta ze starym pakietem: %v; po przełączeniu y wrócił po %s, x (tylko stare CA) połączony ponownie: %v, tls_error=%v; repliki: %v; pakiet po finish: %d CA",
		len(bb.CAs), staleBefore, ms(yBack), xBack, xs.HasEvent("tls_error"), reps, len(nb.CAs))
	if len(bb.CAs) != 2 || !staleBefore || xBack || strings.Join(reps, ",") != "y" || len(nb.CAs) != 1 {
		t.Fatal("rotacja CA nie spełnia kryterium")
	}
}
