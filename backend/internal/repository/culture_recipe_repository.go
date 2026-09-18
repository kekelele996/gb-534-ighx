package repository

import (
	"context"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fmt"
	"gorm.io/gorm"
	"strings"
	"time"
)

type CultureRecipeRepository interface {
	Create(context.Context, *model.CultureRecipe) error
	GetByID(context.Context, uint, bool) (model.CultureRecipe, error)
	List(context.Context, dto.CultureRecipeQuery) ([]model.CultureRecipe, int64, error)
	UpdateWithVersion(context.Context, *model.CultureRecipe, int) (bool, error)
	Transition(context.Context, uint, string, string, int, time.Time) (bool, error)
	MaxVersion(context.Context, uint, string) (int, error)
	ListPublishedInGroup(context.Context, uint, string, uint) ([]model.CultureRecipe, error)
	BulkObsolete(context.Context, []uint, time.Time) (int64, error)
}
type cultureRecipeRepository struct{ db *gorm.DB }

func NewCultureRecipeRepository(db *gorm.DB) CultureRecipeRepository {
	return &cultureRecipeRepository{db: db}
}
func (r *cultureRecipeRepository) Create(ctx context.Context, recipe *model.CultureRecipe) error {
	if err := r.db.WithContext(ctx).Create(recipe).Error; err != nil {
		return fmt.Errorf("create culture recipe: %w", err)
	}
	return nil
}
func (r *cultureRecipeRepository) GetByID(ctx context.Context, id uint, preload bool) (model.CultureRecipe, error) {
	var recipe model.CultureRecipe
	query := r.db.WithContext(ctx)
	if preload {
		query = query.Preload("Vessel")
	}
	if err := query.First(&recipe, id).Error; err != nil {
		return model.CultureRecipe{}, fmt.Errorf("find culture recipe %d: %w", id, err)
	}
	return recipe, nil
}
func (r *cultureRecipeRepository) List(ctx context.Context, query dto.CultureRecipeQuery) ([]model.CultureRecipe, int64, error) {
	base := r.db.WithContext(ctx).Model(&model.CultureRecipe{})
	if query.VesselID != 0 {
		base = base.Where("vessel_id = ?", query.VesselID)
	}
	if query.State != "" {
		base = base.Where("recipe_state = ?", query.State)
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		base = base.Where("LOWER(recipe_code) LIKE ? OR LOWER(organism) LIKE ?", pattern, pattern)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count culture recipes: %w", err)
	}
	var recipes []model.CultureRecipe
	offset := (query.Page - 1) * query.PageSize
	if err := base.Preload("Vessel").Order("recipe_code ASC, version DESC").
		Limit(query.PageSize).Offset(offset).Find(&recipes).Error; err != nil {
		return nil, 0, fmt.Errorf("list culture recipes: %w", err)
	}
	return recipes, total, nil
}
func (r *cultureRecipeRepository) UpdateWithVersion(ctx context.Context, recipe *model.CultureRecipe, expected int) (bool, error) {
	result := r.db.WithContext(ctx).Model(&model.CultureRecipe{}).
		Where("id = ? AND version = ? AND recipe_state IN ?", recipe.ID, expected, []string{"draft", "validated"}).
		Updates(map[string]any{
			"organism": recipe.Organism, "target_duration_h": recipe.TargetDurationH,
			"phase_boundaries_json":  recipe.PhaseBoundariesJSON,
			"reference_curves_json":  recipe.ReferenceCurvesJSON,
			"tolerance_profile_json": recipe.ToleranceProfileJSON,
			"updated_at":             recipe.UpdatedAt,
		})
	if result.Error != nil {
		return false, fmt.Errorf("update culture recipe %d: %w", recipe.ID, result.Error)
	}
	return result.RowsAffected == 1, nil
}
func (r *cultureRecipeRepository) Transition(
	ctx context.Context, id uint, from, to string, version int, updatedAt time.Time,
) (bool, error) {
	result := r.db.WithContext(ctx).Model(&model.CultureRecipe{}).
		Where("id = ? AND recipe_state = ? AND version = ?", id, from, version).
		Updates(map[string]any{"recipe_state": to, "updated_at": updatedAt})
	if result.Error != nil {
		return false, fmt.Errorf("transition culture recipe %d: %w", id, result.Error)
	}
	return result.RowsAffected == 1, nil
}
func (r *cultureRecipeRepository) MaxVersion(ctx context.Context, vesselID uint, recipeCode string) (int, error) {
	var maximum int
	if err := r.db.WithContext(ctx).Model(&model.CultureRecipe{}).
		Where("vessel_id = ? AND recipe_code = ?", vesselID, recipeCode).
		Select("COALESCE(MAX(version), 0)").Scan(&maximum).Error; err != nil {
		return 0, fmt.Errorf("find latest culture recipe version: %w", err)
	}
	return maximum, nil
}

// ListPublishedInGroup returns every currently published version of one recipe
// code on one vessel. excludeID (usually the version about to be published) is
// skipped so callers can collect the versions that must be auto-obsoleted.
func (r *cultureRecipeRepository) ListPublishedInGroup(
	ctx context.Context, vesselID uint, recipeCode string, excludeID uint,
) ([]model.CultureRecipe, error) {
	var recipes []model.CultureRecipe
	query := r.db.WithContext(ctx).
		Where("vessel_id = ? AND recipe_code = ? AND recipe_state = ?", vesselID, recipeCode, "published")
	if excludeID != 0 {
		query = query.Where("id <> ?", excludeID)
	}
	if err := query.Order("version DESC").Find(&recipes).Error; err != nil {
		return nil, fmt.Errorf("list published culture recipe versions: %w", err)
	}
	return recipes, nil
}

// BulkObsolete flips the given published versions to obsolete in one conditional
// update. Rows that are no longer published are left untouched and the affected
// row count is returned so callers can detect concurrent switches.
func (r *cultureRecipeRepository) BulkObsolete(
	ctx context.Context, ids []uint, updatedAt time.Time,
) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := r.db.WithContext(ctx).Model(&model.CultureRecipe{}).
		Where("id IN ? AND recipe_state = ?", ids, "published").
		Updates(map[string]any{"recipe_state": "obsolete", "updated_at": updatedAt})
	if result.Error != nil {
		return 0, fmt.Errorf("bulk obsolete culture recipes: %w", result.Error)
	}
	return result.RowsAffected, nil
}
