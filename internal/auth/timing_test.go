package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/auth"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

type mockUserRepo struct {
	users map[string]*domain.User
	err   error
}

func (m *mockUserRepo) Create(ctx context.Context, u *domain.User) error {
	return m.err
}

func (m *mockUserRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.users[email]
	if !ok {
		return nil, domain.ErrUserNotFound
	}
	return u, nil
}

func (m *mockUserRepo) GetByID(ctx context.Context, id string) (*domain.User, error) {
	return nil, domain.ErrUserNotFound
}

func TestLogin_TimingIndistinguishableOnUnknownUser(t *testing.T) {
	// Generate valid password hash for existing user
	existingPassword := "correct-password-123"
	hash, err := bcrypt.GenerateFromPassword([]byte(existingPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	repo := &mockUserRepo{
		users: map[string]*domain.User{
			"existing@example.com": {
				ID:           "u1",
				Email:        "existing@example.com",
				PasswordHash: string(hash),
			},
		},
	}

	svc := auth.NewService(nil, repo, nil, nil, nil, nil, nil, []byte("test-jwt-secret-key-32-bytes-long!"))

	// 1. Existing user with wrong password
	startExisting := time.Now()
	_, errExisting := svc.Login(context.Background(), "existing@example.com", "wrong-password-123")
	durationExisting := time.Since(startExisting)

	if !errors.Is(errExisting, domain.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for wrong password, got: %v", errExisting)
	}

	// 2. Non-existent user
	startUnknown := time.Now()
	_, errUnknown := svc.Login(context.Background(), "unknown@example.com", "wrong-password-123")
	durationUnknown := time.Since(startUnknown)

	if !errors.Is(errUnknown, domain.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown user, got: %v", errUnknown)
	}

	// Verify both performed bcrypt calculation (both durations must be significantly above 0, e.g. > 5ms)
	if durationUnknown < 5*time.Millisecond {
		t.Errorf("duration for unknown user was %v (< 5ms), bcrypt dummy hash comparison was likely bypassed", durationUnknown)
	}
	if durationExisting < 5*time.Millisecond {
		t.Errorf("duration for existing user was %v (< 5ms)", durationExisting)
	}

	t.Logf("Timing comparison - Existing user wrong password: %v, Unknown user: %v", durationExisting, durationUnknown)
}
