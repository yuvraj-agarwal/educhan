package app

import (
	"strings"
	"testing"
)

func TestLoadMigrations(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) < 2 {
		t.Fatalf("loaded %d migrations, want at least 2", len(migrations))
	}

	for i, m := range migrations {
		if i > 0 && m.version <= migrations[i-1].version {
			t.Errorf("migrations are not ordered: %q follows %q", m.name, migrations[i-1].name)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %q is empty", m.name)
		}
	}
	if migrations[0].version != 1 {
		t.Errorf("first migration version = %d, want 1", migrations[0].version)
	}
}
