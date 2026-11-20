package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	// DefaultSessionTTL is the default lifetime of a login bearer token.
	DefaultSessionTTL = 24 * time.Hour

	maxUsernameBytes = 64
	maxPasswordBytes = 256

	defaultArgonTime    uint32 = 1
	defaultArgonMemory  uint32 = 64 * 1024 // 64 MiB in KiB
	defaultArgonThreads uint8  = 4
	defaultArgonKeyLen  uint32 = 32
	defaultSaltBytes           = 16
	defaultTokenBytes          = 32
)

var (
	ErrInvalidUsername    = errors.New("invalid username")
	ErrInvalidPassword    = errors.New("invalid password")
	ErrUsernameTaken      = errors.New("username already exists")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrUserNotFound       = errors.New("user not found")
)

// Argon2Params holds the Argon2id parameters persisted alongside a user's password hash.
type Argon2Params struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
}

// DefaultArgon2Params returns the standard Argon2id parameters for password hashing.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		Time:    defaultArgonTime,
		Memory:  defaultArgonMemory,
		Threads: defaultArgonThreads,
		KeyLen:  defaultArgonKeyLen,
	}
}

// User represents public account metadata.
type User struct {
	ID        string
	Username  string
	CreatedAt time.Time
}

// StoredUser pairs public account metadata with Argon2id verification material.
type StoredUser struct {
	User
	PasswordHash []byte
	PasswordSalt []byte
	ArgonParams  Argon2Params
}

// StoredSession represents a persisted session identified only by its token digest.
type StoredSession struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AuthResult contains a newly issued bearer token, its expiration, and the authenticated user.
type AuthResult struct {
	Token     string
	ExpiresAt time.Time
	User      User
}

// Store persists user accounts and session digests.
type Store interface {
	CreateUser(ctx context.Context, user StoredUser) error
	GetUserByUsername(ctx context.Context, username string) (StoredUser, error)
	GetUserByID(ctx context.Context, id string) (StoredUser, error)
	CreateSession(ctx context.Context, session StoredSession) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (StoredSession, error)
	DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error
}

// IDGenerator produces unique user identifiers.
type IDGenerator func() (string, error)

// Service manages user registration, Argon2id authentication, and bearer token sessions.
type Service struct {
	store       Store
	idGen       IDGenerator
	sessionTTL  time.Duration
	argonParams Argon2Params
	now         func() time.Time
	dummySalt   []byte
	dummyHash   []byte
}

// Option configures optional settings on Service.
type Option func(*Service)

// WithClock overrides the clock used for timestamps and token expiration checks.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSessionTTL overrides the duration for which newly issued tokens remain valid.
func WithSessionTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.sessionTTL = ttl
		}
	}
}

// WithIDGenerator overrides the user ID generator.
func WithIDGenerator(gen IDGenerator) Option {
	return func(s *Service) {
		if gen != nil {
			s.idGen = gen
		}
	}
}

// WithArgon2Params overrides the Argon2id parameters used when registering new users.
func WithArgon2Params(params Argon2Params) Option {
	return func(s *Service) {
		if params.Time > 0 && params.Memory > 0 && params.Threads > 0 && params.KeyLen > 0 {
			s.argonParams = params
		}
	}
}

// NewService constructs an account and session Service backed by store.
func NewService(store Store, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, errors.New("account store is required")
	}
	s := &Service{
		store:       store,
		idGen:       newUUIDv4,
		sessionTTL:  DefaultSessionTTL,
		argonParams: DefaultArgon2Params(),
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}

	dummySalt := make([]byte, defaultSaltBytes)
	if _, err := rand.Read(dummySalt); err != nil {
		return nil, fmt.Errorf("generate dummy salt: %w", err)
	}
	s.dummySalt = dummySalt
	s.dummyHash = argon2.IDKey(
		[]byte("filedock-dummy-password"),
		dummySalt,
		s.argonParams.Time,
		s.argonParams.Memory,
		s.argonParams.Threads,
		s.argonParams.KeyLen,
	)
	return s, nil
}

// Register creates a new user account with an independent random salt and Argon2id hash.
func (s *Service) Register(ctx context.Context, username, password string) (User, error) {
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}

	id, err := s.idGen()
	if err != nil {
		return User{}, fmt.Errorf("generate user id: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return User{}, errors.New("generated empty user id")
	}

	salt := make([]byte, defaultSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return User{}, fmt.Errorf("generate password salt: %w", err)
	}

	hash := argon2.IDKey(
		[]byte(password),
		salt,
		s.argonParams.Time,
		s.argonParams.Memory,
		s.argonParams.Threads,
		s.argonParams.KeyLen,
	)

	record := StoredUser{
		User: User{
			ID:        id,
			Username:  username,
			CreatedAt: s.now().UTC(),
		},
		PasswordHash: hash,
		PasswordSalt: salt,
		ArgonParams:  s.argonParams,
	}
	if err := s.store.CreateUser(ctx, record); err != nil {
		return User{}, err
	}
	return record.User, nil
}

// Login verifies username and password with Argon2id and creates a new bearer session.
func (s *Service) Login(ctx context.Context, username, password string) (AuthResult, error) {
	if username == "" {
		return AuthResult{}, ErrInvalidUsername
	}
	if password == "" {
		return AuthResult{}, ErrInvalidPassword
	}
	if err := ValidateUsername(username); err != nil || ValidatePassword(password) != nil {
		// Equalize work and return uniform credential failure.
		candidate := argon2.IDKey(
			[]byte(password),
			s.dummySalt,
			s.argonParams.Time,
			s.argonParams.Memory,
			s.argonParams.Threads,
			s.argonParams.KeyLen,
		)
		_ = subtle.ConstantTimeCompare(candidate, s.dummyHash)
		return AuthResult{}, ErrInvalidCredentials
	}

	stored, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			candidate := argon2.IDKey(
				[]byte(password),
				s.dummySalt,
				s.argonParams.Time,
				s.argonParams.Memory,
				s.argonParams.Threads,
				s.argonParams.KeyLen,
			)
			_ = subtle.ConstantTimeCompare(candidate, s.dummyHash)
			return AuthResult{}, ErrInvalidCredentials
		}
		return AuthResult{}, err
	}

	params := stored.ArgonParams
	if params.Time == 0 || params.Memory == 0 || params.Threads == 0 || params.KeyLen == 0 ||
		len(stored.PasswordSalt) == 0 || len(stored.PasswordHash) == 0 {
		return AuthResult{}, errors.New("corrupted stored password parameters")
	}

	candidate := argon2.IDKey(
		[]byte(password),
		stored.PasswordSalt,
		params.Time,
		params.Memory,
		params.Threads,
		params.KeyLen,
	)
	if subtle.ConstantTimeCompare(candidate, stored.PasswordHash) != 1 {
		return AuthResult{}, ErrInvalidCredentials
	}

	rawTokenBytes := make([]byte, defaultTokenBytes)
	if _, err := rand.Read(rawTokenBytes); err != nil {
		return AuthResult{}, fmt.Errorf("generate session token: %w", err)
	}
	rawToken := fmt.Sprintf("%x", rawTokenBytes)
	tokenHash := HashToken(rawToken)

	now := s.now().UTC()
	expiresAt := now.Add(s.sessionTTL)
	session := StoredSession{
		TokenHash: tokenHash,
		UserID:    stored.ID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := s.store.CreateSession(ctx, session); err != nil {
		return AuthResult{}, err
	}

	return AuthResult{
		Token:     rawToken,
		ExpiresAt: expiresAt,
		User:      stored.User,
	}, nil
}

// Authenticate verifies a bearer token and returns the associated user.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (User, error) {
	if !isValidRawToken(rawToken) {
		return User{}, ErrUnauthorized
	}
	tokenHash := HashToken(rawToken)
	session, err := s.store.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return User{}, ErrUnauthorized
		}
		return User{}, err
	}

	if !s.now().UTC().Before(session.ExpiresAt.UTC()) {
		_ = s.store.DeleteSessionByTokenHash(ctx, tokenHash)
		return User{}, ErrUnauthorized
	}

	user, err := s.store.GetUserByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return User{}, ErrUnauthorized
		}
		return User{}, err
	}
	return user.User, nil
}

// Logout revokes the current bearer token immediately. Expired or unknown tokens return ErrUnauthorized.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if !isValidRawToken(rawToken) {
		return ErrUnauthorized
	}
	tokenHash := HashToken(rawToken)
	session, err := s.store.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return ErrUnauthorized
		}
		return err
	}

	expired := !s.now().UTC().Before(session.ExpiresAt.UTC())
	if err := s.store.DeleteSessionByTokenHash(ctx, tokenHash); err != nil {
		return err
	}
	if expired {
		return ErrUnauthorized
	}
	return nil
}

// HashToken returns the lowercase SHA-256 hex digest of a raw bearer token.
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return fmt.Sprintf("%x", sum[:])
}

// ValidateUsername checks that a username is non-empty, within length limits, and free of whitespace or control chars.
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("%w: username must not be empty", ErrInvalidUsername)
	}
	if len(username) > maxUsernameBytes {
		return fmt.Errorf("%w: username exceeds %d bytes", ErrInvalidUsername, maxUsernameBytes)
	}
	for i := 0; i < len(username); i++ {
		b := username[i]
		if b <= 0x20 || b >= 0x7f {
			return fmt.Errorf("%w: username must use printable ASCII without whitespace", ErrInvalidUsername)
		}
	}
	return nil
}

// ValidatePassword checks that a password is non-empty and within byte length limits.
func ValidatePassword(password string) error {
	if password == "" {
		return fmt.Errorf("%w: password must not be empty", ErrInvalidPassword)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("%w: password exceeds %d bytes", ErrInvalidPassword, maxPasswordBytes)
	}
	return nil
}

func isValidRawToken(rawToken string) bool {
	if rawToken == "" || len(rawToken) > 256 {
		return false
	}
	for i := 0; i < len(rawToken); i++ {
		b := rawToken[i]
		if b <= 0x20 || b >= 0x7f {
			return false
		}
	}
	return true
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
