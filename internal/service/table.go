package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

type Table struct {
	store *postgres.Store
	plans *Plan
}

// plans may be nil, which permits everything. See service.Plan.
func NewTable(store *postgres.Store, plans *Plan) *Table {
	return &Table{store: store, plans: plans}
}

func (s *Table) assertRestaurant(ctx context.Context, tenantID, restaurantID uuid.UUID) error {
	_, err := s.store.GetRestaurant(ctx, gen.GetRestaurantParams{ID: restaurantID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("restaurant: %w", postgres.MapError(err))
	}
	return nil
}

// tokenAttempts bounds the retry loop when a freshly generated token collides
// with an existing one. With 32^14 possibilities a single collision is already
// implausible; three in a row means something is wrong with the random source,
// and looping forever would turn that into a hung request.
const tokenAttempts = 3

func (s *Table) Create(ctx context.Context, tenantID, restaurantID uuid.UUID, in domain.TableInput) (domain.Table, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return domain.Table{}, err
	}
	in.Label = strings.TrimSpace(in.Label)
	if err := in.Validate(); err != nil {
		return domain.Table{}, err
	}
	if err := s.plans.Check(ctx, tenantID, domain.ResourceTables); err != nil {
		return domain.Table{}, err
	}

	for attempt := range tokenAttempts {
		token, err := domain.NewQRToken()
		if err != nil {
			return domain.Table{}, fmt.Errorf("generate qr token: %w", err)
		}

		row, err := s.store.CreateTable(ctx, gen.CreateTableParams{
			RestaurantID: restaurantID,
			Label:        in.Label,
			QrToken:      token,
			Seats:        in.Seats,
			SortOrder:    in.SortOrder,
		})
		if err == nil {
			return postgres.ToTable(row), nil
		}

		mapped := postgres.MapError(err)
		// A duplicate label is the caller's problem and must not be retried; a
		// duplicate token is ours, and a fresh one will almost certainly work.
		if errors.Is(mapped, domain.ErrConflict) && isTokenCollision(mapped) && attempt < tokenAttempts-1 {
			continue
		}
		return domain.Table{}, fmt.Errorf("create table: %w", mapped)
	}

	return domain.Table{}, fmt.Errorf("create table: could not find a free qr token in %d attempts", tokenAttempts)
}

func isTokenCollision(err error) bool {
	var statusErr *domain.StatusError
	return errors.As(err, &statusErr) && strings.Contains(statusErr.Message, "token")
}

func (s *Table) Get(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Table, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return domain.Table{}, err
	}
	row, err := s.store.GetTable(ctx, gen.GetTableParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return domain.Table{}, wrap("get table", err)
	}
	return postgres.ToTable(row), nil
}

func (s *Table) List(ctx context.Context, tenantID, restaurantID uuid.UUID) ([]domain.Table, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListTables(ctx, restaurantID)
	if err != nil {
		return nil, wrap("list tables", err)
	}
	return postgres.ToTables(rows), nil
}

type TablePatch struct {
	Label      *string
	Seats      *int32
	ClearSeats bool
	SortOrder  *int32
	IsActive   *bool
}

func (s *Table) Update(ctx context.Context, tenantID, restaurantID, id uuid.UUID, patch TablePatch) (domain.Table, error) {
	current, err := s.Get(ctx, tenantID, restaurantID, id)
	if err != nil {
		return domain.Table{}, err
	}

	merged := domain.TableInput{Label: current.Label, Seats: current.Seats, SortOrder: current.SortOrder}
	if patch.Label != nil {
		merged.Label = strings.TrimSpace(*patch.Label)
	}
	if patch.ClearSeats {
		merged.Seats = nil
	} else if patch.Seats != nil {
		merged.Seats = patch.Seats
	}
	if patch.SortOrder != nil {
		merged.SortOrder = *patch.SortOrder
	}
	if err := merged.Validate(); err != nil {
		return domain.Table{}, err
	}

	params := gen.UpdateTableParams{
		ID:           id,
		RestaurantID: restaurantID,
		SortOrder:    patch.SortOrder,
		IsActive:     patch.IsActive,
		ClearSeats:   patch.ClearSeats,
	}
	if patch.Label != nil {
		params.Label = &merged.Label
	}
	if !patch.ClearSeats {
		params.Seats = patch.Seats
	}

	row, err := s.store.UpdateTable(ctx, params)
	if err != nil {
		return domain.Table{}, wrap("update table", err)
	}
	return postgres.ToTable(row), nil
}

// RotateToken issues a new QR token, which invalidates every code already
// printed for that table. Used when a code leaks or a table is repurposed.
func (s *Table) RotateToken(ctx context.Context, tenantID, restaurantID, id uuid.UUID) (domain.Table, error) {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return domain.Table{}, err
	}

	for attempt := range tokenAttempts {
		token, err := domain.NewQRToken()
		if err != nil {
			return domain.Table{}, fmt.Errorf("generate qr token: %w", err)
		}

		row, err := s.store.RotateTableToken(ctx, gen.RotateTableTokenParams{
			ID: id, RestaurantID: restaurantID, QrToken: token,
		})
		if err == nil {
			return postgres.ToTable(row), nil
		}

		mapped := postgres.MapError(err)
		if errors.Is(mapped, domain.ErrConflict) && attempt < tokenAttempts-1 {
			continue
		}
		return domain.Table{}, fmt.Errorf("rotate table token: %w", mapped)
	}
	return domain.Table{}, fmt.Errorf("rotate table token: could not find a free qr token")
}

func (s *Table) Delete(ctx context.Context, tenantID, restaurantID, id uuid.UUID) error {
	if err := s.assertRestaurant(ctx, tenantID, restaurantID); err != nil {
		return err
	}
	n, err := s.store.DeleteTable(ctx, gen.DeleteTableParams{ID: id, RestaurantID: restaurantID})
	if err != nil {
		return wrap("delete table", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}
