package auth_test

import (
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/auth"
)

func TestValidatePassword_MinLength(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"empty", "", true},
		{"too short - 7 chars", "1234567", true},
		{"too short - 1 char", "a", true},
		{"exact min - 8 chars", "12345678", false},
		{"exact min - 8 complex chars", "Pass!234", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := auth.ValidatePassword(tt.password)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePassword(%q) err = %v, wantErr = %v", tt.password, err, tt.wantErr)
			}
			if tt.wantErr && err != auth.ErrPasswordTooShort {
				t.Errorf("expected ErrPasswordTooShort, got %v", err)
			}
		})
	}
}

func TestValidatePassword_Bcrypt72ByteLimit(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"exact limit 72 ascii chars", strings.Repeat("a", 72), false},
		{"above limit 73 ascii chars", strings.Repeat("a", 73), true},
		{"100 character passphrase", strings.Repeat("b", 100), true},
		{"multi-byte unicode within 72 bytes", "p@sswörd_1234567890", false},
		{"multi-byte unicode exceeding 72 bytes", strings.Repeat("€", 25), true}, // € is 3 bytes each, 25*3 = 75 bytes
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := auth.ValidatePassword(tt.password)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePassword(len=%d bytes) err = %v, wantErr = %v", len([]byte(tt.password)), err, tt.wantErr)
			}
			if tt.wantErr && err != auth.ErrPasswordTooLong {
				t.Errorf("expected ErrPasswordTooLong, got %v", err)
			}
		})
	}
}
