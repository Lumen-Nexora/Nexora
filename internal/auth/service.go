package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/apikey"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"time"
)

// dummyHash is a precomputed bcrypt hash used to perform dummy comparisons
// when a login attempts to authenticate a non-existent account, ensuring
// response times are indistinguishable and preventing timing-based user enumeration.
var dummyHash []byte

func init() {
	var err error
	dummyHash, err = bcrypt.GenerateFromPassword([]byte("nexora-timing-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("failed to generate dummy bcrypt hash: %v", err))
	}
}

type RegisterRequest struct {
	Email       string             `json:"email"`
	Password    string             `json:"password"`
	Name        string             `json:"name"`
	AccountType domain.AccountType `json:"account_type"` // individual | organization
	OrgName     string             `json:"org_name"`     // required if account_type == organization
}

type AuthResponse struct {
	User         *domain.User   `json:"user"`
	Tenant       *domain.Tenant `json:"tenant"`
	Role         string         `json:"role"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
}

type Service interface {
	Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error)
	Login(ctx context.Context, email, password string) (*AuthResponse, error)
	RefreshToken(ctx context.Context, refreshTokenStr string) (*AuthResponse, error)
}

type RefreshTokenStore interface {
	RevokeIfActive(ctx context.Context, token string, expiresAt time.Time) (bool, error)
}

type UserRepo interface {
	Create(ctx context.Context, u *domain.User) error
	GetByEmail(ctx context.Context, email string) (*domain.User, error)
	GetByID(ctx context.Context, id string) (*domain.User, error)
}

type service struct {
	db            postgres.DB
	userRepo      UserRepo
	tenantRepo    *postgres.TenantRepo
	orgRepo       *postgres.OrgRepo
	apiKeyRepo    *postgres.APIKeyRepo
	webhookRepo   *postgres.WebhookRepository
	refreshTokens RefreshTokenStore
	jwtSecret     []byte
}

func NewService(
	db postgres.DB,
	userRepo UserRepo,
	tenantRepo *postgres.TenantRepo,
	orgRepo *postgres.OrgRepo,
	apiKeyRepo *postgres.APIKeyRepo,
	webhookRepo *postgres.WebhookRepository,
	refreshTokens RefreshTokenStore,
	jwtSecret []byte,
) Service {
	return &service{
		db:            db,
		userRepo:      userRepo,
		tenantRepo:    tenantRepo,
		orgRepo:       orgRepo,
		apiKeyRepo:    apiKeyRepo,
		webhookRepo:   webhookRepo,
		refreshTokens: refreshTokens,
		jwtSecret:     jwtSecret,
	}
}

func (s *service) Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error) {
	if req.Email == "" || req.Password == "" || req.Name == "" {
		return nil, errors.New("email, password, and name are required")
	}

	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}

	if req.AccountType == "" {
		req.AccountType = domain.AccountTypeIndividual
	}
	if req.AccountType != domain.AccountTypeIndividual && req.AccountType != domain.AccountTypeOrganization {
		return nil, errors.New("account_type must be 'individual' or 'organization'")
	}
	if req.AccountType == domain.AccountTypeOrganization && req.OrgName == "" {
		return nil, errors.New("org_name is required for organization account registration")
	}

	// Check if email is already taken
	existing, err := s.userRepo.GetByEmail(ctx, req.Email)
	if err == nil && existing != nil {
		return nil, domain.ErrUserAlreadyExists
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	userID := uuid.New().String()
	tenantID := uuid.New().String()
	now := time.Now().UTC()

	user := &domain.User{
		ID:           userID,
		Email:        req.Email,
		PasswordHash: string(hash),
		Name:         req.Name,
		CreatedAt:    now,
	}

	tenantName := req.Name + "'s Tenant"
	if req.AccountType == domain.AccountTypeOrganization {
		tenantName = req.OrgName
	}

	t := &domain.Tenant{
		ID:          tenantID,
		Name:        tenantName,
		Email:       req.Email,
		AccountType: req.AccountType,
		CreatedAt:   now,
	}

	member := &domain.OrgMember{
		ID:        uuid.New().String(),
		TenantID:  tenantID,
		UserID:    userID,
		Role:      domain.RoleOwner,
		CreatedAt: now,
	}

	var raw, prefix string

	if err := postgres.RunInTx(ctx, s.db, func(txCtx context.Context) error {
		if err := s.userRepo.Create(txCtx, user); err != nil {
			return fmt.Errorf("create user: %w", err)
		}

		if err := s.tenantRepo.Create(txCtx, t); err != nil {
			return fmt.Errorf("create tenant: %w", err)
		}

		if err := s.orgRepo.AddMember(txCtx, member); err != nil {
			return fmt.Errorf("add owner member: %w", err)
		}

		var genErr error
		// Onboarding mints a live key. A test key is an explicit, later action
		// so a new tenant cannot accidentally run in sandbox mode.
		raw, prefix, genErr = apikey.Generate(domain.ModeLive)
		if genErr != nil {
			return fmt.Errorf("generate api key: %w", genErr)
		}

		key := &domain.APIKey{
			ID:        uuid.New().String(),
			TenantID:  tenantID,
			KeyHash:   apikey.Hash(raw),
			Prefix:    prefix,
			Mode:      domain.ModeLive,
			Role:      domain.RoleOwner,
			CreatedAt: now,
		}
		if err := s.apiKeyRepo.Create(txCtx, key); err != nil {
			return fmt.Errorf("create api key: %w", err)
		}

		webhookConfig := &domain.WebhookEndpoint{
			ID:        uuid.New().String(),
			TenantID:  &tenantID,
			URL:       "", // to be configured later
			Secret:    uuid.New().String(),
			Active:    false,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.webhookRepo.CreateEndpoint(txCtx, webhookConfig); err != nil {
			return fmt.Errorf("create webhook config: %w", err)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	accessToken, err := GenerateToken(userID, tenantID, domain.RoleOwner, user.Email, "access", s.jwtSecret, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}

	refreshToken, err := GenerateToken(userID, tenantID, domain.RoleOwner, user.Email, "refresh", s.jwtSecret, 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	return &AuthResponse{
		User:         user,
		Tenant:       t,
		Role:         domain.RoleOwner,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *service) Login(ctx context.Context, email, password string) (*AuthResponse, error) {
	if email == "" || password == "" {
		return nil, domain.ErrInvalidCredentials
	}

	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil || user == nil {
		if errors.Is(err, domain.ErrUserNotFound) || (err == nil && user == nil) {
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return nil, domain.ErrInvalidCredentials
		}
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, domain.ErrInvalidCredentials
	}

	member, err := s.orgRepo.GetUserActiveMember(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("get user active tenant: %w", err)
	}

	t, err := s.tenantRepo.GetByID(ctx, member.TenantID)
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}

	accessToken, err := GenerateToken(user.ID, t.ID, member.Role, user.Email, "access", s.jwtSecret, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}

	refreshToken, err := GenerateToken(user.ID, t.ID, member.Role, user.Email, "refresh", s.jwtSecret, 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	return &AuthResponse{
		User:         user,
		Tenant:       t,
		Role:         member.Role,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *service) RefreshToken(ctx context.Context, refreshTokenStr string) (*AuthResponse, error) {
	claims, err := ParseToken(refreshTokenStr, s.jwtSecret)
	if err != nil || claims.TokenType != "refresh" {
		return nil, errors.New("invalid or expired refresh token")
	}

	user, err := s.userRepo.GetByID(ctx, claims.Sub)
	if err != nil {
		return nil, domain.ErrUserNotFound
	}

	member, err := s.orgRepo.GetMember(ctx, claims.TenantID, claims.Sub)
	if err != nil {
		return nil, domain.ErrOrgMemberNotFound
	}

	t, err := s.tenantRepo.GetByID(ctx, claims.TenantID)
	if err != nil {
		return nil, errors.New("tenant not found")
	}

	accessToken, err := GenerateToken(user.ID, t.ID, member.Role, user.Email, "access", s.jwtSecret, 24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}

	newRefreshToken, err := GenerateToken(user.ID, t.ID, member.Role, user.Email, "refresh", s.jwtSecret, 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	if s.refreshTokens == nil {
		return nil, errors.New("refresh token store is not configured")
	}
	active, err := s.refreshTokens.RevokeIfActive(ctx, refreshTokenStr, time.Unix(claims.Exp, 0))
	if err != nil {
		return nil, fmt.Errorf("revoke refresh token: %w", err)
	}
	if !active {
		return nil, errors.New("invalid or expired refresh token")
	}

	return &AuthResponse{
		User:         user,
		Tenant:       t,
		Role:         member.Role,
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
	}, nil
}
