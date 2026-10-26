package cmd

import (
	"strings"
	"testing"
)

func TestCheckPoolMigrationSpace(t *testing.T) {
	const gib = uint64(1 << 30)

	tests := []struct {
		name      string
		available uint64
		total     uint64
		required  uint64
		wantErr   bool
	}{
		{name: "enough space with headroom", available: 3 * gib, total: 4 * gib, required: 2 * gib},
		{name: "insufficient for source", available: gib, total: 4 * gib, required: 2 * gib, wantErr: true},
		{name: "insufficient reserved headroom", available: 2*gib + 1, total: 4 * gib, required: 2 * gib, wantErr: true},
		{name: "pool completely full", available: 0, total: 4 * gib, required: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPoolMigrationSpace(tt.available, tt.total, tt.required)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkPoolMigrationSpace() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "grove pool resize +5") {
				t.Fatalf("actionable resize guidance missing from error: %v", err)
			}
		})
	}
}
