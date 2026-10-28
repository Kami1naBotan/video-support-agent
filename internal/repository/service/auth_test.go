package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"video-support-agent/internal/model"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type authUserStoreStub struct {
	existing  *model.User
	findErr   error
	createErr error
	created   *model.User
}

func (s *authUserStoreStub) FindByUsername(
	_ context.Context,
	_ string,
) (*model.User, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.existing != nil {
		return s.existing, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *authUserStoreStub) Create(
	_ context.Context,
	user *model.User,
) error {
	if s.createErr != nil {
		return s.createErr
	}
	user.ID = 1
	s.created = user
	return nil
}

func TestAuthServiceRegisterSuccess(t *testing.T) {
	store := &authUserStoreStub{}
	svc := NewAuthService(store)
	password := "password123"

	user, err := svc.Register(
		context.Background(),
		"  alice  ",
		password,
	)
	if err != nil {
		t.Fatal(err)
	}

	if user.ID != 1 || user.Username != "alice" {
		t.Fatal("unexpected user identity")
	}
	if user.Role != "user" {
		t.Fatalf("expected role user, got %q", user.Role)
	}
	if store.created != user {
		t.Fatal("user was not passed to the repository")
	}
	if user.PasswordHash == "" || user.PasswordHash == password {
		t.Fatal("password must be stored as a hash")
	}
	if err := bcrypt.CompareHashAndPassword(
		[]byte(user.PasswordHash),
		[]byte(password),
	); err != nil {
		t.Fatal("stored hash does not match the password")
	}
}

func TestAuthServiceRegisterDuplicateUsername(t *testing.T) {
	store := &authUserStoreStub{
		existing: &model.User{ID: 1, Username: "alice"},
	}
	svc := NewAuthService(store)

	_, err := svc.Register(
		context.Background(),
		"alice",
		"password123",
	)
	if !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("expected ErrUsernameExists, got %v", err)
	}
	if store.created != nil {
		t.Fatal("duplicate user should not be created")
	}
}

func TestAuthServiceRegisterInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
	}{
		{"short username", "ab", "password123"},
		{"long username", strings.Repeat("a", 65), "password123"},
		{"short password", "alice", "1234567"},
		{"long password", "alice", strings.Repeat("a", 73)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &authUserStoreStub{}
			svc := NewAuthService(store)

			_, err := svc.Register(
				context.Background(),
				tt.username,
				tt.password,
			)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("expected ErrInvalidArgument, got %v", err)
			}
			if store.created != nil {
				t.Fatal("invalid user should not be created")
			}
		})
	}
}

func TestAuthServiceRegisterStoreErrors(t *testing.T) {
	dbErr := errors.New("database unavailable")

	tests := []struct {
		name  string
		store *authUserStoreStub
	}{
		{"lookup failed", &authUserStoreStub{findErr: dbErr}},
		{"create failed", &authUserStoreStub{createErr: dbErr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewAuthService(tt.store)

			user, err := svc.Register(
				context.Background(),
				"alice",
				"password123",
			)
			if !errors.Is(err, dbErr) {
				t.Fatalf("expected database error, got %v", err)
			}
			if user != nil {
				t.Fatal("failed registration should not return a user")
			}
		})
	}
}

func TestAuthServiceAuthenticate(t *testing.T) {
	password := "password123"
	hash, err := bcrypt.GenerateFromPassword(
		[]byte(password),
		bcrypt.MinCost,
	)
	if err != nil {
		t.Fatal(err)
	}

	existingUser := &model.User{
		ID:           7,
		Username:     "alice",
		PasswordHash: string(hash),
		Role:         "user",
	}
	dbErr := errors.New("database unavailable")

	tests := []struct {
		name     string
		username string
		password string
		store    *authUserStoreStub
		wantErr  error
	}{
		{
			name:     "correct credentials",
			username: "alice",
			password: password,
			store: &authUserStoreStub{
				existing: existingUser,
			},
		},
		{
			name:     "wrong password",
			username: "alice",
			password: "wrongpass123",
			store: &authUserStoreStub{
				existing: existingUser,
			},
			wantErr: ErrInvalidCredentials,
		},
		{
			name:     "unknown username",
			username: "unknown",
			password: password,
			store:    &authUserStoreStub{},
			wantErr:  ErrInvalidCredentials,
		},
		{
			name:     "empty password",
			username: "alice",
			password: "",
			store: &authUserStoreStub{
				existing: existingUser,
			},
			wantErr: ErrInvalidCredentials,
		},
		{
			name:     "database error",
			username: "alice",
			password: password,
			store: &authUserStoreStub{
				findErr: dbErr,
			},
			wantErr: dbErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewAuthService(tt.store)

			user, err := svc.Authenticate(
				context.Background(),
				tt.username,
				tt.password,
			)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if user != nil {
					t.Fatal("failed login should not return a user")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected login error: %v", err)
				}
				if user == nil || user.ID != existingUser.ID {
					t.Fatal("expected the existing user")
				}
				if user.Role != "user" {
					t.Fatalf("unexpected role: %q", user.Role)
				}
			}

			if tt.store.created != nil {
				t.Fatal("login must not create a user")
			}
		})
	}
}
