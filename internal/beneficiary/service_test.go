package beneficiary

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/stellar/go/keypair"
)

type fakeRepo struct {
	items map[string]*domain.Beneficiary
	dup   bool
}

func (f *fakeRepo) Create(_ context.Context, b *domain.Beneficiary) error {
	if f.dup {
		return ErrDuplicate
	}
	if f.items == nil {
		f.items = map[string]*domain.Beneficiary{}
	}
	f.items[b.ID] = b
	return nil
}
func (f *fakeRepo) Get(_ context.Context, id string) (*domain.Beneficiary, error) {
	b, ok := f.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}
func (f *fakeRepo) List(context.Context) ([]*domain.Beneficiary, error) {
	items := make([]*domain.Beneficiary, 0, len(f.items))
	for _, b := range f.items {
		items = append(items, b)
	}
	return items, nil
}
func (f *fakeRepo) Check(_ context.Context, account string) (bool, bool, error) {
	for _, b := range f.items {
		if b.Account == account {
			return true, b.Status == domain.BeneficiaryActive, nil
		}
	}
	return len(f.items) > 0, false, nil
}
func (f *fakeRepo) Activate(_ context.Context, id string, now time.Time) (*domain.Beneficiary, error) {
	b, err := f.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	b.Status = domain.BeneficiaryActive
	b.UpdatedAt = now
	return b, nil
}
func (f *fakeRepo) Revoke(_ context.Context, id string) error {
	b, err := f.Get(context.Background(), id)
	if err != nil {
		return err
	}
	b.Status = domain.BeneficiaryRevoked
	return nil
}

func beneficiaryContext() context.Context {
	return tenant.WithMode(tenant.WithID(context.Background(), "tenant-1"), domain.ModeTest)
}

func TestCreateStartsCoolingOffPeriod(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	svc := NewService(&fakeRepo{}, nil).(*service)
	svc.now = func() time.Time { return now }
	b, err := svc.Create(beneficiaryContext(), keypair.MustRandom().Address(), "Treasury")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != domain.BeneficiaryPending {
		t.Fatalf("status = %q", b.Status)
	}
	if !b.CooldownUntil.Equal(now.Add(CoolingOffPeriod)) {
		t.Fatalf("cooldown = %s", b.CooldownUntil)
	}
}

func TestActivationIsBlockedUntilCooldownEnds(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{}
	svc := NewService(repo, nil).(*service)
	svc.now = func() time.Time { return now }
	b, err := svc.Create(beneficiaryContext(), keypair.MustRandom().Address(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Activate(beneficiaryContext(), b.ID); !errors.Is(err, ErrCoolingOff) {
		t.Fatalf("activate error = %v", err)
	}
	svc.now = func() time.Time { return now.Add(CoolingOffPeriod) }
	active, err := svc.Activate(beneficiaryContext(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != domain.BeneficiaryActive {
		t.Fatalf("status = %q", active.Status)
	}
}

func TestCreateRejectsInvalidAndDuplicateAccounts(t *testing.T) {
	svc := NewService(&fakeRepo{dup: true}, nil)
	if _, err := svc.Create(beneficiaryContext(), "not-an-account", ""); !errors.Is(err, ErrInvalidAccount) {
		t.Fatalf("invalid error = %v", err)
	}
	if _, err := svc.Create(beneficiaryContext(), keypair.MustRandom().Address(), ""); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
}
