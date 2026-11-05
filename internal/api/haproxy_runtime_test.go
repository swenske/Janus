package api

import (
	"context"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

// TestMaskTableKeys: a caller who may not apply a configuration (a
// reader) doesn't see string/binary stick-table keys, which can be a
// client's cookie or credential; an operator does, and address-keyed
// tables are never masked (security review 2026-11-05 #9).
func TestMaskTableKeys(t *testing.T) {
	reader := context.WithValue(context.Background(), callerKey{}, Caller{Name: "r", Roles: []string{pki.RoleReader}})
	operator := context.WithValue(context.Background(), callerKey{}, Caller{Name: "o", Roles: []string{pki.RoleOperator}})
	for _, tc := range []struct {
		ctx     context.Context
		keyType string
		mask    bool
	}{
		{reader, "string", true},
		{reader, "binary", true},
		{reader, "ip", false},
		{reader, "ipv6", false},
		{reader, "integer", false},
		{operator, "string", false},
		{operator, "binary", false},
	} {
		if got := maskTableKeys(tc.ctx, tc.keyType); got != tc.mask {
			t.Errorf("maskTableKeys(%s, %q) = %v, want %v", tc.ctx.Value(callerKey{}).(Caller).Name, tc.keyType, got, tc.mask)
		}
	}
}
