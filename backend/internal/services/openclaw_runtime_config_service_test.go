package services

import (
	"context"
	"strings"
	"testing"

	"clawreef/internal/models"
)

// TestUpdateRuntimeConfig_ValidationRejectsBadInput exercises the
// pre-flight validation paths in UpdateRuntimeConfig that do not
// require a live K8s client. Once a payload passes validation the
// method does need a client; we stop short of that.
func TestUpdateRuntimeConfig_ValidationRejectsBadInput(t *testing.T) {
	svc := &openClawRuntimeConfigService{} // pod/exec services nil; never reached
	runningInstance := &models.Instance{ID: 42, UserID: 1, Status: "running"}

	cases := []struct {
		name     string
		instance *models.Instance
		payload  []byte
		wantErr  string
	}{
		{
			name:     "nil instance",
			instance: nil,
			payload:  []byte(`[{"path":"x","value":"y"}]`),
			wantErr:  "instance is required",
		},
		{
			name:     "empty payload",
			instance: runningInstance,
			payload:  nil,
			wantErr:  "batch JSON payload is required",
		},
		{
			name:     "payload over size limit",
			instance: runningInstance,
			payload:  bigJSONArray(openclawRuntimeConfigMaxBytes + 1),
			wantErr:  "too large",
		},
		{
			name:     "not valid JSON",
			instance: runningInstance,
			payload:  []byte(`{not json`),
			wantErr:  "invalid batch JSON",
		},
		{
			name:     "JSON object instead of array",
			instance: runningInstance,
			payload:  []byte(`{"path":"x","value":"y"}`),
			wantErr:  "invalid batch JSON",
		},
		{
			name:     "empty array",
			instance: runningInstance,
			payload:  []byte(`[]`),
			wantErr:  "must contain at least one config entry",
		},
		{
			name:     "entry missing path",
			instance: runningInstance,
			payload:  []byte(`[{"value":"y"}]`),
			wantErr:  "missing required 'path'",
		},
		{
			name:     "entry missing value",
			instance: runningInstance,
			payload:  []byte(`[{"path":"x"}]`),
			wantErr:  "missing required 'value'",
		},
		{
			name:     "instance not running",
			instance: &models.Instance{ID: 7, UserID: 1, Status: "stopped"},
			payload:  []byte(`[{"path":"x","value":"y"}]`),
			wantErr:  "is not running",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.UpdateRuntimeConfig(context.Background(), tc.instance, tc.payload)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

// bigJSONArray builds a syntactically-valid JSON array whose serialized
// length is at least minBytes. It pads the array with simple no-op
// entries until the threshold is crossed.
func bigJSONArray(minBytes int) []byte {
	var b strings.Builder
	b.WriteByte('[')
	first := true
	for b.Len() < minBytes {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(`{"path":"x","value":"`)
		// 256-byte filler per entry keeps the loop O(minBytes/256).
		for i := 0; i < 256; i++ {
			b.WriteByte('a')
		}
		b.WriteString(`"}`)
	}
	b.WriteByte(']')
	return []byte(b.String())
}
