package tests

import (
	"errors"
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
	_, _ = h.Run(nil, "docker", "rm", "-f", "ep-sidecar")
	t.Cleanup(func() { h.Run(nil, "docker", "rm", "-f", "ep-sidecar") })
	if _, err := h.Run(strings.NewReader(connect), "sh", "-c",
		`docker run -d --name ep-sidecar --network ep-e2e_epnet --dns 172.28.0.2 -e ENTRYPOINT_CONNECT="$(cat)" ep-e2e-connector --upstream http://172.28.0.21:9100 --health / --replica sidecar --drain-timeout 5s`); err != nil {
		t.Fatal(err)
	}
	e.WaitReplicas("api", []string{"sidecar"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/", 200)
	_, body, _ := get(e.HTTPClient(), "https://api.ep.test/")
	inspect, _ := h.Run(nil, "docker", "inspect", "-f", "{{.Config.User}} {{range .Config.Env}}{{.}} {{end}}", "ep-sidecar")
	user := strings.Fields(inspect)[0]
	t0 := time.Now()
	h.Run(nil, "docker", "stop", "-t", "30", "ep-sidecar")
	stopped := time.Since(t0)
	e.WaitReplicas("api", nil, 5*time.Second)
	code, _ := h.Run(nil, "docker", "inspect", "-f", "{{.State.ExitCode}}", "ep-sidecar")
	logs, _ := h.Run(nil, "docker", "logs", "ep-sidecar")
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
