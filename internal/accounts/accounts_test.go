package accounts

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type memoryStore struct {
	usersByID   map[string]StoredUser
	usersByName map[string]StoredUser
	sessions    map[string]StoredSession
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		usersByID:   make(map[string]StoredUser),
		usersByName: make(map[string]StoredUser),
		sessions:    make(map[string]StoredSession),
	}
}

func (m *memoryStore) CreateUser(_ context.Context, user StoredUser) error {
	if _, exists := m.usersByName[user.Username]; exists {
		return ErrUsernameTaken
	}
	m.usersByID[user.ID] = user
	m.usersByName[user.Username] = user
	return nil
}

func (m *memoryStore) GetUserByUsername(_ context.Context, username string) (StoredUser, error) {
	u, ok := m.usersByName[username]
	if !ok {
		return StoredUser{}, ErrUserNotFound
	}
	return u, nil
}

func (m *memoryStore) GetUserByID(_ context.Context, id string) (StoredUser, error) {
	u, ok := m.usersByID[id]
	if !ok {
		return StoredUser{}, ErrUserNotFound
	}
	return u, nil
}

func (m *memoryStore) CreateSession(_ context.Context, session StoredSession) error {
	m.sessions[session.TokenHash] = session
	return nil
}

func (m *memoryStore) GetSessionByTokenHash(_ context.Context, tokenHash string) (StoredSession, error) {
	s, ok := m.sessions[tokenHash]
	if !ok {
		return StoredSession{}, ErrUnauthorized
	}
	return s, nil
}

func (m *memoryStore) DeleteSessionByTokenHash(_ context.Context, tokenHash string) error {
	if _, ok := m.sessions[tokenHash]; !ok {
		return ErrUnauthorized
	}
	delete(m.sessions, tokenHash)
	return nil
}

var fastArgonParams = Argon2Params{
	Time:    1,
	Memory:  8 * 1024,
	Threads: 1,
	KeyLen:  32,
}

func TestRegisterLoginAuthenticateLogoutAndExpiry(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	now := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

	svc, err := NewService(
		store,
		WithClock(func() time.Time { return now }),
		WithSessionTTL(1*time.Hour),
		WithArgon2Params(fastArgonParams),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	user1, err := svc.Register(ctx, "alice", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Register(alice) error = %v", err)
	}
	if user1.ID == "" || user1.Username != "alice" {
		t.Fatalf("unexpected user: %+v", user1)
	}

	// Duplicate username is rejected.
	if _, err := svc.Register(ctx, "alice", "other-pass"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("Register(duplicate) = %v, want ErrUsernameTaken", err)
	}

	// Second user with identical password gets an independent random salt and different hash.
	user2, err := svc.Register(ctx, "bob", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Register(bob) error = %v", err)
	}
	storedAlice := store.usersByID[user1.ID]
	storedBob := store.usersByID[user2.ID]
	if bytes.Equal(storedAlice.PasswordSalt, storedBob.PasswordSalt) {
		t.Fatal("expected distinct random salts per user")
	}
	if bytes.Equal(storedAlice.PasswordHash, storedBob.PasswordHash) {
		t.Fatal("expected distinct password hashes for users with different salts")
	}
	if storedAlice.ArgonParams != fastArgonParams {
		t.Fatalf("persisted ArgonParams = %+v, want %+v", storedAlice.ArgonParams, fastArgonParams)
	}

	// Wrong password and unknown user both return ErrInvalidCredentials.
	if _, err := svc.Login(ctx, "alice", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login(wrong password) = %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.Login(ctx, "nonexistent", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login(unknown user) = %v, want ErrInvalidCredentials", err)
	}

	// Valid login issues bearer token and stores only its digest.
	auth, err := svc.Login(ctx, "alice", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Login(alice) error = %v", err)
	}
	if auth.Token == "" {
		t.Fatal("expected non-empty bearer token")
	}
	if _, rawStored := store.sessions[auth.Token]; rawStored {
		t.Fatal("raw token must not be used as session key in store")
	}
	if _, hashStored := store.sessions[HashToken(auth.Token)]; !hashStored {
		t.Fatal("expected SHA-256 token digest in session store")
	}

	authedUser, err := svc.Authenticate(ctx, auth.Token)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if authedUser.ID != user1.ID {
		t.Fatalf("Authenticate() user ID = %q, want %q", authedUser.ID, user1.ID)
	}

	// Logout immediately revokes the token.
	if err := svc.Logout(ctx, auth.Token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := svc.Authenticate(ctx, auth.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate(after logout) = %v, want ErrUnauthorized", err)
	}
	if err := svc.Logout(ctx, auth.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("second Logout() = %v, want ErrUnauthorized", err)
	}

	// Expired token is rejected with ErrUnauthorized.
	auth2, err := svc.Login(ctx, "alice", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("second Login() error = %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := svc.Authenticate(ctx, auth2.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate(expired) = %v, want ErrUnauthorized", err)
	}
}
