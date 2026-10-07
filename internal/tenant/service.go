package tenant

import (
	"context"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

type Repository interface {
	GetByID(ctx context.Context, id string) (*domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

type Service interface {
	GetByID(ctx context.Context, id string) (*domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) GetByID(ctx context.Context, id string) (*domain.Tenant, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *service) Update(ctx context.Context, t *domain.Tenant) error {
	return s.repo.Update(ctx, t)
}
