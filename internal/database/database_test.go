package database

import (
	"context"
	"testing"
)

func TestDBNilBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		db   *DB
	}{
		{name: "nil receiver"},
		{name: "nil pool", db: &DB{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.db.Ping(context.Background()); err == nil {
				t.Fatalf("Ping() error = %v, want uninitialized pool error", err)
			}
			if stats := tt.db.Stats(); stats != nil {
				t.Fatalf("Stats() = %#v, want nil", stats)
			}
			tt.db.Close()
		})
	}
}
