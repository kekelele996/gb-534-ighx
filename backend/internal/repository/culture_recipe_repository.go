package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CultureRecipeRepository interface {
	Create(context.Context, *model.CultureRecipe) error
	GetByID(context.Context, uint, bool) (model.CultureRecipe, error)
	LockByID(context.Context, uint) (model.CultureRecipe, error)
	List(context.Context, dto.CultureRecipeQuery) ([]model.CultureRecipe, int64, error)
	UpdateWithVersion(context.Context, *model.CultureRecipe, int) (bool, error)
	Transition(context.Context, uint, string, string, int, time.Time) (bool, error)
	ConditionalTransition(context.Context, uint, string, string, time.Time) (bool, error)
	LockPublishedByGroup(context.Context, uint, string) ([]model.CultureRecipe, error)
	ListPublishedVersions(context.Context, uint, string) ([]model.CultureRecipe, error)
	MaxVersion(context.Context, uint, string) (int, error)
}

// RecipePublishGateRepository serializes recipe version switches for one
// vessel + recipe-code group.
type RecipePublishGateRepository interface {
	Epoch(context.Context, uint, string) (int, error)
	EnsureAndLock(context.Context, uint, string, time.Time) (model.RecipePublishGate, error)
	BumpEpoch(context.Context, uint, string, int, time.Time) (bool, error)
}

// TxRunner executes a unit of work in a database transaction; the context
// passed to fn carries the transaction so sharing repositories join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(context.Context) error) error
}

type cultureRecipeRepository struct{ db *gorm.DB }
type recipePublishGateRepository struct{ db *gorm.DB }
type txRunner struct{ db *gorm.DB }

func NewCultureRecipeRepository(db *gorm.DB) CultureRecipeRepository {
	return &cultureRecipeRepository{db: db}
}
func NewRecipePublishGateRepository(db *gorm.DB) RecipePublishGateRepository {
	return &recipePublishGateRepository{db: db}
}
func NewTxRunner(db *gorm.DB) TxRunner { return &txRunner{db: db} }

// --- context transaction and locking primitives shared by repositories ---

type txContextKey struct{}

// WithTx attaches an in-flight transaction to the context; repositories then
// run every statement inside that transaction across service boundaries.
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

func conn(db *gorm.DB, ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txContextKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return db.WithContext(ctx)
}

// lockForUpdate adds FOR UPDATE on PostgreSQL. SQLite (in-memory tests and
// runtime smoke) uses one serialized writer and does not support the clause.
func lockForUpdate(query *gorm.DB) *gorm.DB {
	if query.Dialector.Name() == "postgres" {
		return query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return query
}

// ErrLockContention marks a lost database serialization point (row lock, gate
// epoch, single-writer lock); services map it to HTTP 409 refresh-and-retry.
var ErrLockContention = lockContentionError{}

type lockContentionError struct{}

func (lockContentionError) Error() string { return "database lock contention detected" }

// asLockContention classifies driver lock failures (SQLite SQLITE_BUSY in
// single-writer test databases, PostgreSQL lock timeout / serialization).
func asLockContention(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "lock timeout") ||
		strings.Contains(message, "deadlock detected") ||
		strings.Contains(message, "could not serialize access") {
		return lockContentionWrap{cause: err}
	}
	return err
}

type lockContentionWrap struct{ cause error }

func (e lockContentionWrap) Error() string        { return e.cause.Error() }
func (e lockContentionWrap) Unwrap() error        { return e.cause }
func (e lockContentionWrap) Is(target error) bool { return target == ErrLockContention }

func (r *txRunner) InTx(ctx context.Context, fn func(context.Context) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(WithTx(ctx, tx))
	})
}

// --- culture recipe repository ---

func (r *cultureRecipeRepository) Create(ctx context.Context, recipe *model.CultureRecipe) error {
	if err := conn(r.db, ctx).Create(recipe).Error; err != nil {
		return fmt.Errorf("create culture recipe: %w", err)
	}
	return nil
}

func (r *cultureRecipeRepository) GetByID(ctx context.Context, id uint, preload bool) (model.CultureRecipe, error) {
	var recipe model.CultureRecipe
	query := conn(r.db, ctx)
	if preload {
		query = query.Preload("Vessel")
	}
	if err := query.First(&recipe, id).Error; err != nil {
		return model.CultureRecipe{}, fmt.Errorf("find culture recipe %d: %w", id, err)
	}
	return recipe, nil
}

// LockByID reads a recipe row and locks it for the surrounding transaction;
// without a transaction it behaves like GetByID.
func (r *cultureRecipeRepository) LockByID(ctx context.Context, id uint) (model.CultureRecipe, error) {
	var recipe model.CultureRecipe
	if err := lockForUpdate(conn(r.db, ctx)).First(&recipe, id).Error; err != nil {
		return model.CultureRecipe{}, fmt.Errorf("lock culture recipe %d: %w", id, err)
	}
	return recipe, nil
}

func (r *cultureRecipeRepository) List(ctx context.Context, query dto.CultureRecipeQuery) ([]model.CultureRecipe, int64, error) {
	base := conn(r.db, ctx).Model(&model.CultureRecipe{})
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
	result := conn(r.db, ctx).Model(&model.CultureRecipe{}).
		Where("id = ? AND version = ? AND recipe_state IN ?", recipe.ID, expected, []string{"draft", "validated"}).
		Updates(map[string]any{
			"organism": recipe.Organism, "target_duration_h": recipe.TargetDurationH,
			"phase_boundaries_json":  recipe.PhaseBoundariesJSON,
			"reference_curves_json":  recipe.ReferenceCurvesJSON,
			"tolerance_profile_json": recipe.ToleranceProfileJSON,
			"updated_at":            recipe.UpdatedAt,
		})
	if result.Error != nil {
		return false, fmt.Errorf("update culture recipe %d: %w", recipe.ID, result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (r *cultureRecipeRepository) Transition(
	ctx context.Context, id uint, from, to string, version int, updatedAt time.Time,
) (bool, error) {
	result := conn(r.db, ctx).Model(&model.CultureRecipe{}).
		Where("id = ? AND recipe_state = ? AND version = ?", id, from, version).
		Updates(map[string]any{"recipe_state": to, "updated_at": updatedAt})
	if result.Error != nil {
		return false, fmt.Errorf("transition culture recipe %d: %w", id, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// ConditionalTransition is an optimistic compare-and-swap on state only; it is
// the publish-gate serialization primitive.
func (r *cultureRecipeRepository) ConditionalTransition(
	ctx context.Context, id uint, from, to string, updatedAt time.Time,
) (bool, error) {
	result := conn(r.db, ctx).Model(&model.CultureRecipe{}).
		Where("id = ? AND recipe_state = ?", id, from).
		Updates(map[string]any{"recipe_state": to, "updated_at": updatedAt})
	if result.Error != nil {
		return false, fmt.Errorf("conditional transition culture recipe %d: %w", id, result.Error)
	}
	return result.RowsAffected == 1, nil
}

// LockPublishedByGroup locks every published version in a vessel+code group,
// giving a release a stable predecessor set to obsolete atomically.
func (r *cultureRecipeRepository) LockPublishedByGroup(ctx context.Context, vesselID uint, recipeCode string) ([]model.CultureRecipe, error) {
	var recipes []model.CultureRecipe
	if err := lockForUpdate(conn(r.db, ctx)).
		Where("vessel_id = ? AND recipe_code = ? AND recipe_state = ?", vesselID, recipeCode, "published").
		Order("version ASC").Find(&recipes).Error; err != nil {
		return nil, fmt.Errorf("lock published recipes for %s: %w", recipeCode, err)
	}
	return recipes, nil
}

// ListPublishedVersions returns published versions in a group without locks,
// for the read-model gate projection.
func (r *cultureRecipeRepository) ListPublishedVersions(ctx context.Context, vesselID uint, recipeCode string) ([]model.CultureRecipe, error) {
	var recipes []model.CultureRecipe
	if err := conn(r.db, ctx).
		Where("vessel_id = ? AND recipe_code = ? AND recipe_state = ?", vesselID, recipeCode, "published").
		Order("version ASC").Find(&recipes).Error; err != nil {
		return nil, fmt.Errorf("list published recipes for %s: %w", recipeCode, err)
	}
	return recipes, nil
}

func (r *cultureRecipeRepository) MaxVersion(ctx context.Context, vesselID uint, recipeCode string) (int, error) {
	var maximum int
	if err := conn(r.db, ctx).Model(&model.CultureRecipe{}).
		Where("vessel_id = ? AND recipe_code = ?", vesselID, recipeCode).
		Select("COALESCE(MAX(version), 0)").Scan(&maximum).Error; err != nil {
		return 0, fmt.Errorf("find latest culture recipe version: %w", err)
	}
	return maximum, nil
}

// --- recipe publish gate repository ---

func (r *recipePublishGateRepository) Epoch(ctx context.Context, vesselID uint, recipeCode string) (int, error) {
	var gate model.RecipePublishGate
	err := conn(r.db, ctx).
		Where("vessel_id = ? AND recipe_code = ?", vesselID, recipeCode).First(&gate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read recipe publish gate epoch: %w", err)
	}
	return gate.Epoch, nil
}

func (r *recipePublishGateRepository) EnsureAndLock(
	ctx context.Context, vesselID uint, recipeCode string, now time.Time,
) (model.RecipePublishGate, error) {
	db := conn(r.db, ctx)
	var gate model.RecipePublishGate
	err := lockForUpdate(db).
		Where("vessel_id = ? AND recipe_code = ?", vesselID, recipeCode).First(&gate).Error
	if err == nil {
		return gate, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.RecipePublishGate{}, asLockContention(fmt.Errorf("lock recipe publish gate for %s: %w", recipeCode, err))
	}
	// First release for a group: materialize the gate row, then re-lock.
	if insertErr := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.RecipePublishGate{
		VesselID: vesselID, RecipeCode: recipeCode, Epoch: 0, UpdatedAt: now,
	}).Error; insertErr != nil {
		return model.RecipePublishGate{}, asLockContention(fmt.Errorf("materialize recipe publish gate: %w", insertErr))
	}
	if err := lockForUpdate(db).
		Where("vessel_id = ? AND recipe_code = ?", vesselID, recipeCode).
		First(&gate).Error; err != nil {
		return model.RecipePublishGate{}, asLockContention(fmt.Errorf("lock recipe publish gate for %s: %w", recipeCode, err))
	}
	return gate, nil
}

func (r *recipePublishGateRepository) BumpEpoch(
	ctx context.Context, vesselID uint, recipeCode string, expected int, now time.Time,
) (bool, error) {
	result := conn(r.db, ctx).Model(&model.RecipePublishGate{}).
		Where("vessel_id = ? AND recipe_code = ? AND epoch = ?", vesselID, recipeCode, expected).
		Updates(map[string]any{"epoch": gorm.Expr("epoch + 1"), "updated_at": now})
	if result.Error != nil {
		return false, asLockContention(fmt.Errorf("bump recipe publish gate epoch: %w", result.Error))
	}
	return result.RowsAffected == 1, nil
}
