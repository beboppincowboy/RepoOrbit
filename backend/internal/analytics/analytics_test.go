package analytics

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLoadRejectsUnsupportedWindow(t *testing.T) {
	if _, err := Load(context.Background(), nil, 1, 8, time.Now()); !errors.Is(err, ErrInvalidDays) {
		t.Fatalf("expected ErrInvalidDays, got %v", err)
	}
}
