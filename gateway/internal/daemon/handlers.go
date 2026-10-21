package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/spec"
	"github.com/k18k/entrypoint/gateway/internal/store"
	"github.com/k18k/entrypoint/gateway/internal/token"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

type ctxKey int

const peerKey ctxKey = 1

// peerContext remembers the UID of the process on the other end of the socket (the revision author).
func peerContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ctx
	}
	var cred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) {
		cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cred == nil {
		return ctx
	}
	name := "uid:" + strconv.Itoa(int(cred.Uid))
	if u, err := user.LookupId(strconv.Itoa(int(cred.Uid))); err == nil {
		name = u.Username
	}
	return context.WithValue(ctx, peerKey, name)
}

func author(r *http.Request) string {
	if v, ok := r.Context().Value(peerKey).(string); ok {
		return "cli:" + v
	}
	return "cli"
}

func (d *Daemon) apiHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/status", d.hStatus)
	m.HandleFunc("GET /v1/revisions", d.hRevisions)
	m.HandleFunc("GET /v1/revisions/{num}", d.hRevision)
	m.HandleFunc("GET /v1/services", d.hServices)
	m.HandleFunc("POST /v1/services", d.hServiceAdd)
	m.HandleFunc("DELETE /v1/services/{name}", d.hServiceRm)
	m.HandleFunc("GET /v1/routes", d.hRoutes)
	m.HandleFunc("POST /v1/routes", d.hRouteAdd)
	m.HandleFunc("DELETE /v1/routes/{id}", d.hRouteRm)
	m.HandleFunc("GET /v1/tokens", d.hTokens)
	m.HandleFunc("POST /v1/tokens", d.hTokenCreate)
	m.HandleFunc("POST /v1/tokens/{id}/revoke", d.hTokenRevoke)
	m.HandleFunc("GET /v1/ca", d.hCA)
	m.HandleFunc("POST /v1/ca/rotate", d.hCARotate)
	return m
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, &api.Error{Message: err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, 400, fmt.Errorf("invalid request: %v", err))
		return false
	}
	return true
}

type userError struct{ error }

func badRequest(f string, a ...any) error { return userError{fmt.Errorf(f, a...)} }

// replyChange turns a change result into an HTTP response.
func (d *Daemon) replyChange(w http.ResponseWriter, rev *store.Revision, err error, res *api.ChangeResult) {
	if res == nil {
		res = &api.ChangeResult{}
	}
	res.Revision = rev
	if rev == nil {
		var ue userError
		var ve *spec.ValidationError
		switch {
		case errors.Is(err, store.ErrConflict):
			fail(w, 409, err)
		case errors.As(err, &ue), errors.As(err, &ve):
			fail(w, 400, err)
		case err != nil && strings.HasPrefix(err.Error(), "Caddy rejected"):
			fail(w, 400, err)
		default:
			fail(w, 500, err)
		}
		return
	}
	if err != nil {
		res.Applied = false
		res.Error = err.Error()
		res.Token = nil
		reply(w, 500, &api.Error{Message: "revision " + strconv.FormatInt(rev.Num, 10) + " was not applied: " + err.Error(), Result: res})
		return
	}
	res.Applied = true
	reply(w, 200, res)
}

func (d *Daemon) hRevisions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	revs, err := d.st.Revisions(r.Context(), limit)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if revs == nil {
		revs = []*store.Revision{}
	}
	reply(w, 200, revs)
}

func (d *Daemon) hRevision(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("num"), 10, 64)
	if err != nil {
		fail(w, 400, errors.New("invalid revision number"))
		return
	}
	rev, err := d.st.Revision(r.Context(), n)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if rev == nil {
		fail(w, 404, fmt.Errorf("revision %d does not exist", n))
		return
	}
	reply(w, 200, rev)
}

func (d *Daemon) headSpec(ctx context.Context) (*store.Revision, error) {
	h, err := d.st.Head(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return &store.Revision{Spec: &spec.Spec{}}, nil
	}
	return h, nil
}

// replicas returns the replicas grouped by service, from frps.
func (d *Daemon) replicas(ctx context.Context) (map[string][]api.ReplicaInfo, error) {
	online, err := d.frps.Online(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]api.ReplicaInfo{}
	for _, p := range online {
		parts := strings.SplitN(p.Name, ".", 3)
		if len(parts) != 3 {
			continue
		}
		out[parts[0]] = append(out[parts[0]], api.ReplicaInfo{Name: p.Name, TokenID: parts[1], Status: p.Status, CurConns: p.CurConns, Since: p.LastStartTime})
	}
	return out, nil
}

func (d *Daemon) servicesInfo(ctx context.Context, s *spec.Spec) ([]api.ServiceInfo, bool) {
	reps, err := d.replicas(ctx)
	frpsOK := err == nil
	var out []api.ServiceInfo
	for _, svc := range s.Services {
		si := api.ServiceInfo{Name: svc.Name, Port: svc.Port, Health: svc.Health, Replicas: reps[svc.Name]}
		if si.Replicas == nil {
			si.Replicas = []api.ReplicaInfo{}
		}
		for _, rt := range s.Routes {
			if rt.Service == svc.Name {
				si.Routes = append(si.Routes, rt.Host+rt.Prefix)
			}
		}
		out = append(out, si)
	}
	if out == nil {
		out = []api.ServiceInfo{}
	}
	return out, frpsOK
}

func (d *Daemon) hStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	head, err := d.headSpec(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	last, _ := d.st.LastApplied(ctx)
	comps, _ := d.st.Components(ctx)
	st := api.Status{
		Version:     version.Version,
		GatewayName: d.cfg.GatewayName,
		TunnelPort:  d.cfg.TunnelPort,
		LastApplied: last,
		AuthzRev:    d.authz.Revision(),
		Warnings:    []string{},
	}
	if head.Num > 0 {
		st.Head = head
	}
	for _, c := range comps {
		st.Components = append(st.Components, api.ComponentInfo{Name: c.Name, Applied: c.Applied})
	}
	st.Services, st.FrpsOK = d.servicesInfo(ctx, head.Spec)
	_, cerr := d.caddy.Config(ctx)
	st.CaddyOK = cerr == nil
	if !st.FrpsOK {
		st.Warnings = append(st.Warnings, "frps not responding: replica state unknown")
	} else {
		for _, s := range st.Services {
			if len(s.Replicas) == 0 {
				st.Warnings = append(st.Warnings, fmt.Sprintf("service %s has no healthy replica", s.Name))
			}
		}
	}
	if !st.CaddyOK {
		st.Warnings = append(st.Warnings, "Caddy is not responding on the admin API")
	}
	if last != nil && head.Num != last.Num {
		st.Warnings = append(st.Warnings, fmt.Sprintf("revision %d is not fully applied (status %s)", head.Num, head.Status))
	}
	for _, c := range comps {
		if last != nil && c.Applied != last.Num {
			st.Warnings = append(st.Warnings, fmt.Sprintf("component %s has revision %d, expected %d", c.Name, c.Applied, last.Num))
		}
	}
	if revs, err := d.st.Revisions(ctx, 1); err == nil && len(revs) == 1 && revs[0].Status == store.StatusFailed {
		st.Warnings = append(st.Warnings, fmt.Sprintf("the last revision %d failed: %s", revs[0].Num, revs[0].Error))
	}
	reply(w, 200, st)
}

func (d *Daemon) hServices(w http.ResponseWriter, r *http.Request) {
	head, err := d.headSpec(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	out, _ := d.servicesInfo(r.Context(), head.Spec)
	reply(w, 200, out)
}

func (d *Daemon) hServiceAdd(w http.ResponseWriter, r *http.Request) {
	var req api.ServiceAdd
	if !decode(w, r, &req) {
		return
	}
	var created spec.Service
	rev, err := d.change(r.Context(), req.BaseRev, author(r), func(tx *store.Tx, s *spec.Spec) (string, error) {
		if err := spec.ValidServiceName(req.Name); err != nil {
			return "", userError{err}
		}
		if err := spec.ValidHealthPath(req.Health); err != nil {
			return "", userError{err}
		}
		if s.ServiceByName(req.Name) != nil {
			return "", badRequest("service %q already exists", req.Name)
		}
		id, port, err := tx.ReservePort(req.Name, d.cfg.PortPool.Start, d.cfg.PortPool.End)
		if err != nil {
			return "", err
		}
		created = spec.Service{ID: id, Name: req.Name, Port: port, Health: req.Health}
		s.Services = append(s.Services, created)
		return fmt.Sprintf("add service %s (port %d)", req.Name, port), nil
	})
	d.replyChange(w, rev, err, &api.ChangeResult{Service: &api.ServiceInfo{Name: created.Name, Port: created.Port, Health: created.Health, Replicas: []api.ReplicaInfo{}}})
}

func baseRevFromQuery(r *http.Request) (*int64, error) {
	v := r.URL.Query().Get("baseRev")
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, errors.New("invalid baseRev")
	}
	return &n, nil
}

func (d *Daemon) hServiceRm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	base, err := baseRevFromQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	rev, err := d.change(r.Context(), base, author(r), func(tx *store.Tx, s *spec.Spec) (string, error) {
		svc := s.ServiceByName(name)
		if svc == nil {
			return "", badRequest("service %q does not exist", name)
		}
		var used []string
		for _, rt := range s.Routes {
			if rt.Service == name {
				used = append(used, rt.ID+" ("+rt.Host+rt.Prefix+")")
			}
		}
		if len(used) > 0 {
			return "", badRequest("service %q is used by routes: %s; remove them first", name, strings.Join(used, ", "))
		}
		id := svc.ID
		var svcs []spec.Service
		for _, x := range s.Services {
			if x.ID != id {
				svcs = append(svcs, x)
			}
		}
		s.Services = svcs
		// The service's tokens go away with it; they are bound to the service ID,
		// so a new service with the same name does not inherit them.
		var toks []spec.Token
		for _, t := range s.Tokens {
			if t.ServiceID != id {
				toks = append(toks, t)
			}
		}
		s.Tokens = toks
		return fmt.Sprintf("remove service %s (port %d stays reserved)", name, svc.Port), nil
	})
	d.replyChange(w, rev, err, nil)
}

func routeInfo(rt spec.Route) api.RouteInfo {
	return api.RouteInfo{ID: rt.ID, Host: rt.Host, Prefix: rt.Prefix, Service: rt.Service, Upstream: rt.Upstream,
		StripPrefix: rt.StripPrefix, MaxBodyBytes: rt.EffectiveMaxBody(), ResponseHeaderTimeout: rt.EffectiveHeaderTimeout(), ACME: rt.ACME,
		TLSNames: rt.TLSNames, TLSOnDemand: rt.TLSOnDemand}
}

func (d *Daemon) hRoutes(w http.ResponseWriter, r *http.Request) {
	head, err := d.headSpec(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	out := []api.RouteInfo{}
	for _, rt := range head.Spec.Routes {
		out = append(out, routeInfo(rt))
	}
	reply(w, 200, out)
}

func newRouteID(s *spec.Spec) string {
	for {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		id := "r" + hex.EncodeToString(b)
		if s.RouteByID(id) == nil {
			return id
		}
	}
}

func (d *Daemon) hRouteAdd(w http.ResponseWriter, r *http.Request) {
	var req api.RouteAdd
	if !decode(w, r, &req) {
		return
	}
	var created spec.Route
	rev, err := d.change(r.Context(), req.BaseRev, author(r), func(tx *store.Tx, s *spec.Spec) (string, error) {
		host, err := spec.NormalizeHost(req.Host)
		if err != nil {
			return "", userError{err}
		}
		prefix, err := spec.NormalizePrefix(req.Prefix)
		if err != nil {
			return "", userError{err}
		}
		if (req.Service == "") == (req.Upstream == "") {
			return "", badRequest("pass exactly one of --service or --to")
		}
		if req.Service != "" && s.ServiceByName(req.Service) == nil {
			return "", badRequest("service %q does not exist", req.Service)
		}
		if req.Upstream != "" {
			if err := spec.ValidUpstream(req.Upstream); err != nil {
				return "", userError{err}
			}
		}
		for _, rt := range s.Routes {
			if rt.Host == host && strings.EqualFold(rt.Prefix, prefix) {
				return "", badRequest("route conflict: %s%s is already taken by route %s", host, prefix, rt.ID)
			}
			if rt.Host == host && rt.ACME != req.ACME {
				return "", badRequest("host %s already has routes in ACME mode %q", host, rt.ACME)
			}
		}
		created = spec.Route{ID: newRouteID(s), Host: host, Prefix: prefix, Service: req.Service, Upstream: req.Upstream,
			StripPrefix: req.StripPrefix, MaxBodyBytes: req.MaxBodyBytes, ResponseHeaderTimeout: req.ResponseHeaderTimeout, ACME: req.ACME,
			TLSNames: req.TLSNames, TLSOnDemand: req.TLSOnDemand}
		s.Routes = append(s.Routes, created)
		return fmt.Sprintf("add route %s %s%s -> %s", created.ID, host, prefix, created.Target()), nil
	})
	ri := routeInfo(created)
	d.replyChange(w, rev, err, &api.ChangeResult{Route: &ri})
}

func (d *Daemon) hRouteRm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base, err := baseRevFromQuery(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	rev, err := d.change(r.Context(), base, author(r), func(tx *store.Tx, s *spec.Spec) (string, error) {
		rt := s.RouteByID(id)
		if rt == nil {
			return "", badRequest("route %q does not exist", id)
		}
		desc := fmt.Sprintf("remove route %s %s%s", id, rt.Host, rt.Prefix)
		var out []spec.Route
		for _, x := range s.Routes {
			if x.ID != id {
				out = append(out, x)
			}
		}
		s.Routes = out
		return desc, nil
	})
	d.replyChange(w, rev, err, nil)
}

func (d *Daemon) tokenInfos(ctx context.Context) ([]api.TokenInfo, error) {
	head, err := d.headSpec(ctx)
	if err != nil {
		return nil, err
	}
	revs, err := d.st.Revocations(ctx)
	if err != nil {
		return nil, err
	}
	revokedAt := map[string]time.Time{}
	for _, rv := range revs {
		if _, ok := revokedAt[rv.TokenID]; !ok {
			revokedAt[rv.TokenID] = rv.RevokedAt
		}
	}
	logins, _ := d.st.Logins(ctx)
	out := []api.TokenInfo{}
	for _, t := range head.Spec.Tokens {
		ti := api.TokenInfo{ID: t.ID, Scope: t.Scope(head.Spec), Label: t.Label, CreatedAt: t.CreatedAt, MaxReplicas: t.EffectiveMaxReplicas()}
		if svc := head.Spec.ServiceByID(t.ServiceID); svc != nil {
			ti.Service = svc.Name
		}
		if at, ok := revokedAt[t.ID]; ok {
			ti.Revoked = true
			ti.RevokedAt = &at
		}
		if l, ok := logins[t.ID]; ok {
			ti.LastLogin = &l
		}
		out = append(out, ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (d *Daemon) hTokens(w http.ResponseWriter, r *http.Request) {
	out, err := d.tokenInfos(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	reply(w, 200, out)
}

func (d *Daemon) hTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req api.TokenCreate
	if !decode(w, r, &req) {
		return
	}
	if req.MaxReplicas < 0 || req.MaxReplicas > 1000 {
		fail(w, 400, errors.New("invalid replica limit"))
		return
	}
	if len(req.Label) > 100 {
		fail(w, 400, errors.New("label is too long"))
		return
	}
	full, id, hash, err := token.New()
	if err != nil {
		fail(w, 500, err)
		return
	}
	maxRep := req.MaxReplicas
	if maxRep == 0 {
		maxRep = d.cfg.Limits.ReplicasPerToken
	}
	rev, err := d.change(r.Context(), req.BaseRev, author(r), func(tx *store.Tx, s *spec.Spec) (string, error) {
		svc := s.ServiceByName(req.Service)
		if svc == nil {
			return "", badRequest("service %q does not exist", req.Service)
		}
		s.Tokens = append(s.Tokens, spec.Token{ID: id, ServiceID: svc.ID, Hash: hash, Label: req.Label, CreatedAt: time.Now().UTC(), MaxReplicas: maxRep})
		return fmt.Sprintf("issue token %s for service %s", id, req.Service), nil
	})
	var res *api.ChangeResult
	if err == nil && rev != nil {
		cas, cerr := d.bundleCAs()
		if cerr != nil {
			err = cerr
		} else {
			b := token.Bundle{V: 1, Server: d.cfg.GatewayName, Port: d.cfg.TunnelPort, CAs: cas, Service: req.Service, Token: full}
			enc, eerr := b.Encode()
			if eerr != nil {
				err = eerr
			}
			res = &api.ChangeResult{Token: &api.TokenCreated{ID: id, Service: req.Service, Connect: enc}}
		}
	}
	d.replyChange(w, rev, err, res)
}

func (d *Daemon) hTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.TokenRevoke
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	known := false
	revs, err := d.st.Revisions(ctx, 1000000)
	if err != nil {
		fail(w, 500, err)
		return
	}
	for _, rv := range revs {
		if rv.Spec.TokenByID(id) != nil {
			known = true
			break
		}
	}
	if !known {
		fail(w, 404, fmt.Errorf("token %q does not exist", id))
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "revoked by " + author(r)
	}
	// 1. Revocation journal (outside revisions). 2. Immediate in-memory snapshot swap.
	if err := d.st.Revoke(ctx, id, reason); err != nil {
		fail(w, 500, err)
		return
	}
	d.authz.Revoke(id)
	d.log.Warn("token revoked", "token_id", id)
	reply(w, 200, map[string]any{"revoked": id, "revokedAt": time.Now().UTC()})
}
