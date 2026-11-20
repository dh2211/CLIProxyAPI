package store

import (
	"path/filepath"
	"testing"
)

func TestPostgresStoreResolvesNestedDeleteID(t *testing.T) {
	s := &PostgresStore{authDir: t.TempDir()}
	id := "team/credential.json"
	want := filepath.Join(s.authDir, filepath.FromSlash(id))
	for _, arg := range []string{id, filepath.FromSlash(id), want} {
		got, errResolve := s.resolveDeletePath(arg)
		if errResolve != nil || got != want {
			t.Errorf("resolveDeletePath(%q) = %q, %v; want %q", arg, got, errResolve, want)
		}
	}
}
