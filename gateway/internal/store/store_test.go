package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/spec"
)

func TestRevisionsPortsRevocations(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	add := func(base int64, name string) (*Revision, error) {
		return st.Create(ctx, base, "test", func(tx *Tx, s *spec.Spec) (string, error) {
			id, port, err := tx.ReservePort(name, 20000, 20002)
			if err != nil {
				return "", err
			}
			s.Services = append(s.Services, spec.Service{ID: id, Name: name, Port: port})
			return "add " + name, nil
		}, nil)
	}
	r1, err := add(-1, "a")
	if err != nil || r1.Num != 1 {
		t.Fatal(r1, err)
	}
	if _, err := add(0, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("nieaktualne baseRev: %v", err)
	}
	r2, err := add(1, "b")
	if err != nil || r2.Spec.ServiceByName("b").Port != 20001 {
		t.Fatal(err)
	}
	// Rewizja failed nie jest głową; port z nieudanej rewizji zostaje zarezerwowany.
	st.SetStatus(ctx, 2, StatusFailed, "x")
	h, _ := st.Head(ctx)
	if h.Num != 1 {
		t.Fatalf("głowa %d", h.Num)
	}
	r3, err := add(1, "c")
	if err != nil || r3.Spec.ServiceByName("c").Port != 20002 {
		t.Fatalf("port nie powinien wrócić do puli: %v", err)
	}
	if _, err := add(3, "d"); err == nil {
		t.Fatal("pula wyczerpana, a rezerwacja się udała")
	}
	if _, err := st.db.Exec("DELETE FROM ports"); err == nil {
		t.Fatal("rezerwacje portów usuwalne")
	}
	st.Revoke(ctx, "t1", "test")
	if _, err := st.db.Exec("DELETE FROM revocations"); err == nil {
		t.Fatal("odwołania usuwalne")
	}
	if _, err := st.db.Exec("UPDATE revocations SET token_id = 'x'"); err == nil {
		t.Fatal("odwołania modyfikowalne")
	}
	rv, _ := st.RevokedSet(ctx)
	if !rv["t1"] {
		t.Fatal("brak odwołania")
	}
}
