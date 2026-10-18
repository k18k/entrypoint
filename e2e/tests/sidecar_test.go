package tests

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Konektor jako kontener obok aplikacji (wzorzec z README): ruch, SIGTERM -> drain -> wyjście.
func TestSDK_DockerSidecar(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	// Aplikacja w innym kontenerze (tu: prosty serwer w kliencie a), konektor w osobnym kontenerze.
	e.MustSh(h.ClientA, `nohup node -e "require('http').createServer((q,s)=>{s.end(JSON.stringify({replica:'sidecar'}))}).listen(9100,'0.0.0.0')" > /tmp/sidecar-app.log 2>&1 &`)
	sidecar := fmt.Sprintf("ep%d-sidecar", h.Shard)
	_, _ = h.Run(nil, "docker", "rm", "-f", sidecar)
	t.Cleanup(func() { h.Run(nil, "docker", "rm", "-f", sidecar) })
	if _, err := h.Run(strings.NewReader(connect), "sh", "-c",
		fmt.Sprintf(`docker run -d --name %s --network %s --dns %s -e ENTRYPOINT_CONNECT="$(cat)" ep-e2e-connector --upstream http://%s:9100 --health / --replica sidecar --drain-timeout 5s`, sidecar, h.Network, h.DNSIP, h.IP(21))); err != nil {
		t.Fatal(err)
	}
	e.WaitReplicas("api", []string{"sidecar"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/", 200)
	_, body, _ := get(e.HTTPClient(), "https://api.ep.test/")
	inspect, _ := h.Run(nil, "docker", "inspect", "-f", "{{.Config.User}} {{range .Config.Env}}{{.}} {{end}}", sidecar)
	user := strings.Fields(inspect)[0]
	t0 := time.Now()
	h.Run(nil, "docker", "stop", "-t", "30", sidecar)
	stopped := time.Since(t0)
	e.WaitReplicas("api", nil, 5*time.Second)
	code, _ := h.Run(nil, "docker", "inspect", "-f", "{{.State.ExitCode}}", sidecar)
	logs, _ := h.Run(nil, "docker", "logs", sidecar)
	cl := strings.Count(logs, connect)
	result(t, "kontener konektora (użytkownik %s): odpowiedź %s; docker stop -> drain i wyjście po %s, kod %s; pakiet w logach kontenera: %d",
		user, strings.TrimSpace(body), ms(stopped), strings.TrimSpace(code), cl)
	if replicaOf(body) != "sidecar" || strings.TrimSpace(code) != "0" || stopped > 15*time.Second || cl > 0 {
		t.Fatal("konektor w kontenerze nie działa zgodnie z README")
	}
	if !strings.Contains(logs, `"event":"withdrawn"`) {
		t.Fatal(errors.New("brak wycofania z grupy przy docker stop"))
	}
}
