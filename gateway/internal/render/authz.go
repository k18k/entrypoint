package render

import (
	"encoding/json"

	"github.com/k18k/entrypoint/gateway/internal/spec"
)

// AuthzSnapshot is the data the frps plugin answers from, without touching the database.
type AuthzSnapshot struct {
	Revision int64                   `json:"revision"`
	Services map[string]AuthzService `json:"services"` // key: service name
	Tokens   map[string]AuthzToken   `json:"tokens"`   // key: token ID
}

type AuthzService struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	GroupKey string `json:"groupKey"`
}

type AuthzToken struct {
	Hash        string `json:"hash"`
	ServiceID   int64  `json:"serviceId"`
	Service     string `json:"service"`
	MaxReplicas int    `json:"maxReplicas"`
}

// Group returns the service's frp group name.
func (s AuthzService) Group() string { return "svc-" + s.Name }

// Authz renders the authorization snapshot from the configuration.
func Authz(rev int64, s *spec.Spec) AuthzSnapshot {
	snap := AuthzSnapshot{Revision: rev, Services: map[string]AuthzService{}, Tokens: map[string]AuthzToken{}}
	for _, svc := range s.Services {
		snap.Services[svc.Name] = AuthzService{ID: svc.ID, Name: svc.Name, Port: svc.Port, GroupKey: groupKey(svc)}
	}
	for _, t := range s.Tokens {
		svc := s.ServiceByID(t.ServiceID)
		if svc == nil {
			continue
		}
		snap.Tokens[t.ID] = AuthzToken{Hash: t.Hash, ServiceID: t.ServiceID, Service: svc.Name, MaxReplicas: t.EffectiveMaxReplicas()}
	}
	return snap
}

// The group key is always overridden by the plugin, so clients neither know nor need it.
func groupKey(s spec.Service) string {
	b, _ := json.Marshal([]any{s.ID, s.Name, s.Port})
	return "k" + shortHash(b)
}
