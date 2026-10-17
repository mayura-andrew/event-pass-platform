package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// FieldStore is implemented by postgres.FieldRepo.
type FieldStore interface {
	ListFields(ctx context.Context) ([]domain.FieldDefinition, error)
	ListFieldsWithUsage(ctx context.Context) ([]domain.FieldDefinition, error)
	ScannerFields(ctx context.Context) ([]domain.ScannerField, error)
	CountFields(ctx context.Context) (int, error)
	CreateField(ctx context.Context, f domain.NewField) (*domain.FieldDefinition, error)
	UpdateField(ctx context.Context, key string, u domain.FieldUpdateRequest) (*domain.FieldDefinition, error)
	DeleteField(ctx context.Context, key string) (int, error)
}

// FieldService manages custom delegate fields: what each key of
// custom_attributes is called, its type, and whether scanners and the
// dashboard filter bar show it.
type FieldService struct{ store FieldStore }

func NewFieldService(store FieldStore) *FieldService { return &FieldService{store: store} }

// List returns every field with usage counts and filter options.
func (s *FieldService) List(ctx context.Context) ([]domain.FieldDefinition, error) {
	return s.store.ListFieldsWithUsage(ctx)
}

// ScannerFields returns the labels a scanner needs for its popup.
func (s *FieldService) ScannerFields(ctx context.Context) ([]domain.ScannerField, error) {
	return s.store.ScannerFields(ctx)
}

// Create adds a field by hand (before any CSV has the column, or for the
// single-entry form).
func (s *FieldService) Create(ctx context.Context, actorID string, req domain.FieldCreateRequest) (*domain.FieldDefinition, error) {
	key := strings.TrimSpace(req.Key)
	if key == "" {
		key = NormalizeFieldKey(req.Label)
	}
	if key != NormalizeFieldKey(key) || key == "" {
		return nil, fmt.Errorf("%w: field key must be lower case letters, digits and underscores, starting with a letter (e.g. food_preference)", domain.ErrValidation)
	}
	if domain.ReservedFieldKeys[key] || headerAliases[key] != "" {
		return nil, fmt.Errorf("%w: %s is a built-in delegate column, not a custom field", domain.ErrValidation, key)
	}
	label, err := normalizeFieldLabel(req.Label)
	if err != nil {
		return nil, err
	}
	dataType := strings.ToLower(strings.TrimSpace(req.DataType))
	if dataType == "" {
		dataType = domain.FieldText
	}
	if !domain.ValidFieldType(dataType) {
		return nil, fmt.Errorf("%w: data_type must be text, number, boolean or date", domain.ErrValidation)
	}
	n, err := s.store.CountFields(ctx)
	if err != nil {
		return nil, err
	}
	if n >= domain.MaxFieldDefinitions {
		return nil, fmt.Errorf("%w: at most %d custom fields; delete one you no longer need first", domain.ErrValidation, domain.MaxFieldDefinitions)
	}
	order := domain.DefaultFieldSortOrder
	if req.SortOrder != nil {
		order = *req.SortOrder
	}

	f, err := s.store.CreateField(ctx, domain.NewField{
		Key:                   key,
		Label:                 label,
		DataType:              dataType,
		ShowInScannerPopup:    req.ShowInScannerPopup,
		ShowInDashboardFilter: req.ShowInDashboardFilter,
		SortOrder:             order,
		Source:                domain.FieldSourceManual,
		CreatedBy:             actorID,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("field created", "actor", actorID, "field", f.Key, "scanner", f.ShowInScannerPopup)
	return f, nil
}

// Update changes a field's label, type (only while unused), toggles or order.
func (s *FieldService) Update(ctx context.Context, actorID, key string, req domain.FieldUpdateRequest) (*domain.FieldDefinition, error) {
	if NormalizeFieldKey(key) != key {
		return nil, domain.ErrNotFound
	}
	if req.Label != nil {
		l, err := normalizeFieldLabel(*req.Label)
		if err != nil {
			return nil, err
		}
		req.Label = &l
	}
	if req.DataType != nil {
		t := strings.ToLower(strings.TrimSpace(*req.DataType))
		if !domain.ValidFieldType(t) {
			return nil, fmt.Errorf("%w: data_type must be text, number, boolean or date", domain.ErrValidation)
		}
		req.DataType = &t
	}
	f, err := s.store.UpdateField(ctx, key, req)
	if err != nil {
		return nil, err
	}
	// Scanner visibility decides what is copied to every phone: keep a trail.
	slog.Info("field updated", "actor", actorID, "field", f.Key,
		"scanner", f.ShowInScannerPopup, "filter", f.ShowInDashboardFilter, "type", f.DataType)
	return f, nil
}

// Delete removes a field and all its values. Returns how many delegates had one.
func (s *FieldService) Delete(ctx context.Context, actorID, key string) (int, error) {
	if NormalizeFieldKey(key) != key {
		return 0, domain.ErrNotFound
	}
	n, err := s.store.DeleteField(ctx, key)
	if err != nil {
		return 0, err
	}
	slog.Info("field deleted", "actor", actorID, "field", key, "values_removed", n)
	return n, nil
}

func normalizeFieldLabel(raw string) (string, error) {
	l := strings.Join(strings.Fields(raw), " ")
	if l == "" || utf8.RuneCountInString(l) > domain.MaxFieldLabelLen || !ValidDisplayName(l) {
		return "", fmt.Errorf("%w: label must be 1-%d characters", domain.ErrValidation, domain.MaxFieldLabelLen)
	}
	return l, nil
}

// ---- Dashboard filter validation -------------------------------------------------

// validateCustomFilters checks each filter against the field definitions
// (the key must exist and be enabled as a dashboard filter) and converts its
// value to the field's type. Nothing user-supplied reaches SQL unchecked.
func validateCustomFilters(in []domain.CustomFilter, fields []domain.FieldDefinition) ([]domain.CustomFilter, error) {
	if len(in) > domain.MaxCustomFilters {
		return nil, fmt.Errorf("%w: at most %d custom field filters", domain.ErrValidation, domain.MaxCustomFilters)
	}
	byKey := make(map[string]domain.FieldDefinition, len(fields))
	for _, f := range fields {
		byKey[f.Key] = f
	}
	out := make([]domain.CustomFilter, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, f := range in {
		f.Key = strings.TrimSpace(f.Key)
		def, ok := byKey[f.Key]
		if !ok || !def.ShowInDashboardFilter {
			return nil, fmt.Errorf("%w: %q is not a filterable field", domain.ErrValidation, f.Key)
		}
		if seen[f.Key] {
			return nil, fmt.Errorf("%w: %s is filtered twice", domain.ErrValidation, def.Label)
		}
		seen[f.Key] = true

		f.Op = strings.ToLower(strings.TrimSpace(f.Op))
		if f.Op == "" {
			f.Op = domain.CustomEquals
		}
		switch f.Op {
		case domain.CustomMissing:
			f.Value, f.Typed = "", nil
		case domain.CustomContains:
			if def.DataType != domain.FieldText {
				return nil, fmt.Errorf("%w: %s can only be matched exactly", domain.ErrValidation, def.Label)
			}
			f.Value = strings.TrimSpace(f.Value)
			if f.Value == "" {
				continue // an empty search box filters nothing
			}
			if utf8.RuneCountInString(f.Value) > maxSearchLen {
				return nil, fmt.Errorf("%w: %s filter is too long", domain.ErrValidation, def.Label)
			}
		case domain.CustomEquals:
			v, err := CoerceCustomValue(def.DataType, f.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: %s %v", domain.ErrValidation, def.Label, err)
			}
			if v == nil {
				continue // "any value": no filter
			}
			f.Typed = v
		default:
			return nil, fmt.Errorf("%w: op must be eq, contains or missing", domain.ErrValidation)
		}
		out = append(out, f)
	}
	return out, nil
}
