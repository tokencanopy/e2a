package main

import (
	"errors"
	"testing"

	"github.com/tokencanopy/e2a/internal/limits"
)

func TestScheduledSendMonthlyQuotaResult(t *testing.T) {
	transient := errors.New("limits unavailable")
	tests := []struct {
		name     string
		err      error
		wantOver bool
		wantErr  error
	}{
		{name: "under cap", err: nil},
		{
			name:     "monthly message cap",
			err:      &limits.LimitExceededError{Resource: "messages_month", Limit: 100, Current: 100},
			wantOver: true,
		},
		{
			name: "storage cap does not cancel an accepted send",
			err:  &limits.LimitExceededError{Resource: "storage_bytes", Limit: 1024, Current: 1024},
		},
		{name: "transient error fails open to caller", err: transient, wantErr: transient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			over, err := scheduledSendMonthlyQuotaResult(tt.err)
			if over != tt.wantOver {
				t.Fatalf("over = %v, want %v", over, tt.wantOver)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
