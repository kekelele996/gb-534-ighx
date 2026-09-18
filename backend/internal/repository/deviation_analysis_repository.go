package repository
import (
	"context"
	"fmt"
	"strings"
	"time"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"gorm.io/gorm"
)
type DeviationAnalysisRepository interface {
	Create(context.Context, *model.DeviationAnalysis) error
	GetByID(context.Context, uint, bool) (model.DeviationAnalysis, error)
	List(context.Context, dto.DeviationAnalysisQuery) ([]model.DeviationAnalysis, int64, error)
	FindByIdempotencyKey(context.Context, string) (model.DeviationAnalysis, error)
	FindByInput(context.Context, string, string) (model.DeviationAnalysis, error)
	Transition(context.Context, uint, string, string, map[string]any) (bool, error)
	Complete(context.Context, uint, map[string]any) (bool, error)
	SetReplayVerified(context.Context, uint, bool) error
	ListOpenReferences(context.Context, []uint) ([]OpenAnalysisReference, error)
	LockReferences(context.Context, []uint) error
}
type deviationAnalysisRepository struct{ db *gorm.DB }
func NewDeviationAnalysisRepository(db *gorm.DB) DeviationAnalysisRepository {
	return &deviationAnalysisRepository{db: db}
}
func (r *deviationAnalysisRepository) Create(ctx context.Context, analysis *model.DeviationAnalysis) error {
	if err := conn(r.db, ctx).Create(analysis).Error; err != nil {
		return fmt.Errorf("create deviation analysis: %w", err)
	}
	return nil
}
func (r *deviationAnalysisRepository) GetByID(ctx context.Context, id uint, preload bool) (model.DeviationAnalysis, error) {
	var analysis model.DeviationAnalysis
	query := conn(r.db, ctx)
	if preload {
		query = query.Preload("SensorSeries").Preload("SensorSeries.Vessel").Preload("SensorSeries.Recipe")
	}
	if err := query.First(&analysis, id).Error; err != nil {
		return model.DeviationAnalysis{}, fmt.Errorf("find deviation analysis %d: %w", id, err)
	}
	return analysis, nil
}
func (r *deviationAnalysisRepository) FindByIdempotencyKey(ctx context.Context, key string) (model.DeviationAnalysis, error) {
	var analysis model.DeviationAnalysis
	if err := conn(r.db, ctx).Where("idempotency_key = ?", key).First(&analysis).Error; err != nil {
		return model.DeviationAnalysis{}, fmt.Errorf("find analysis by idempotency key: %w", err)
	}
	return analysis, nil
}
func (r *deviationAnalysisRepository) FindByInput(ctx context.Context, hash, version string) (model.DeviationAnalysis, error) {
	var analysis model.DeviationAnalysis
	if err := conn(r.db, ctx).Where("input_hash = ? AND algorithm_version = ?", hash, version).
		First(&analysis).Error; err != nil {
		return model.DeviationAnalysis{}, fmt.Errorf("find analysis by frozen input: %w", err)
	}
	return analysis, nil
}
func (r *deviationAnalysisRepository) List(ctx context.Context, query dto.DeviationAnalysisQuery) ([]model.DeviationAnalysis, int64, error) {
	base := conn(r.db, ctx).Model(&model.DeviationAnalysis{})
	if query.SensorSeriesID != 0 {
		base = base.Where("sensor_series_id = ?", query.SensorSeriesID)
	}
	if query.RecipeID != 0 {
		base = base.Where("recipe_id = ?", query.RecipeID)
	}
	if query.State != "" {
		base = base.Where("analysis_state = ?", query.State)
	}
	if query.Level != "" {
		base = base.Where("deviation_level = ?", query.Level)
	}
	if query.Initiator != "" {
		base = base.Where("LOWER(initiated_by_name) LIKE ?", "%"+strings.ToLower(query.Initiator)+"%")
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count deviation analyses: %w", err)
	}
	var analyses []model.DeviationAnalysis
	offset := (query.Page - 1) * query.PageSize
	if err := base.Preload("SensorSeries").Preload("SensorSeries.Vessel").Preload("SensorSeries.Recipe").
		Order("analyzed_at DESC, id DESC").Limit(query.PageSize).Offset(offset).Find(&analyses).Error; err != nil {
		return nil, 0, fmt.Errorf("list deviation analyses: %w", err)
	}
	return analyses, total, nil
}
func (r *deviationAnalysisRepository) Transition(
	ctx context.Context, id uint, from, to string, updates map[string]any,
) (bool, error) {
	if updates == nil {
		updates = map[string]any{}
	}
	updates["analysis_state"] = to
	updates["updated_at"] = time.Now().UTC()
	result := conn(r.db, ctx).Model(&model.DeviationAnalysis{}).
		Where("id = ? AND analysis_state = ?", id, from).Updates(updates)
	if result.Error != nil {
		return false, fmt.Errorf("transition deviation analysis %d: %w", id, result.Error)
	}
	return result.RowsAffected == 1, nil
}
func (r *deviationAnalysisRepository) Complete(ctx context.Context, id uint, updates map[string]any) (bool, error) {
	updates["analysis_state"] = "completed"
	updates["updated_at"] = time.Now().UTC()
	result := conn(r.db, ctx).Model(&model.DeviationAnalysis{}).
		Where("id = ? AND analysis_state = ?", id, "analyzing").Updates(updates)
	if result.Error != nil {
		return false, fmt.Errorf("complete deviation analysis %d: %w", id, result.Error)
	}
	return result.RowsAffected == 1, nil
}
func (r *deviationAnalysisRepository) SetReplayVerified(ctx context.Context, id uint, passed bool) error {
	if err := conn(r.db, ctx).Model(&model.DeviationAnalysis{}).Where("id = ?", id).
		Updates(map[string]any{"replay_verified": passed, "updated_at": time.Now().UTC()}).Error; err != nil {
		return fmt.Errorf("store replay verification for analysis %d: %w", id, err)
	}
	return nil
}
type UserRepository interface {
	FindByUsername(context.Context, string) (model.User, error)
}
type userRepository struct{ db *gorm.DB }
func NewUserRepository(db *gorm.DB) UserRepository { return &userRepository{db: db} }
func (r *userRepository) FindByUsername(ctx context.Context, username string) (model.User, error) {
	var user model.User
	if err := conn(r.db, ctx).Where("username = ?", username).First(&user).Error; err != nil {
		return model.User{}, fmt.Errorf("find user by username: %w", err)
	}
	return user, nil
}
type AuditQuery struct {
	EntityType, RequestID, Action string
	ActorID                       uint
	From, To                      *time.Time
	Page, PageSize                int
}
type AuditRepository interface {
	Record(context.Context, model.AuditLog) error
	List(context.Context, AuditQuery) ([]model.AuditLog, int64, error)
}
type auditRepository struct{ db *gorm.DB }
func NewAuditRepository(db *gorm.DB) AuditRepository { return &auditRepository{db: db} }
func (r *auditRepository) Record(ctx context.Context, audit model.AuditLog) error {
	if err := conn(r.db, ctx).Create(&audit).Error; err != nil {
		return fmt.Errorf("record audit log: %w", err)
	}
	return nil
}
func (r *auditRepository) List(ctx context.Context, query AuditQuery) ([]model.AuditLog, int64, error) {
	base := conn(r.db, ctx).Model(&model.AuditLog{})
	if query.EntityType != "" {
		base = base.Where("entity_type = ?", query.EntityType)
	}
	if query.RequestID != "" {
		base = base.Where("request_id = ?", query.RequestID)
	}
	if query.Action != "" {
		base = base.Where("action = ?", query.Action)
	}
	if query.ActorID != 0 {
		base = base.Where("actor_id = ?", query.ActorID)
	}
	if query.From != nil {
		base = base.Where("created_at >= ?", *query.From)
	}
	if query.To != nil {
		base = base.Where("created_at <= ?", *query.To)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}
	var logs []model.AuditLog
	offset := (query.Page - 1) * query.PageSize
	if err := base.Order("created_at DESC, id DESC").Limit(query.PageSize).Offset(offset).Find(&logs).Error; err != nil {
		return nil, 0, fmt.Errorf("list audit logs: %w", err)
	}
	return logs, total, nil
}

// OpenAnalysisStates lists live analysis states that keep a published recipe
// version active: anything not yet confirmed or voided.
var OpenAnalysisStates = []string{
	"queued", "analyzing", "completed", "failed", "reviewed", "investigating",
}

// OpenAnalysisReference is a non-terminal deviation analysis that pins a
// published recipe version and blocks its automatic obsolescence.
type OpenAnalysisReference struct {
	ID              uint      `gorm:"column:id"`
	RecipeID        uint      `gorm:"column:recipe_id"`
	SensorSeriesID  uint      `gorm:"column:sensor_series_id"`
	RunCode         string    `gorm:"column:run_code"`
	AnalysisState   string    `gorm:"column:analysis_state"`
	InitiatedByName string    `gorm:"column:initiated_by_name"`
	AnalyzedAt      time.Time `gorm:"column:analyzed_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// ListOpenReferences returns non-terminal analyses bound to the recipe
// versions, newest first.
func (r *deviationAnalysisRepository) ListOpenReferences(ctx context.Context, recipeIDs []uint) ([]OpenAnalysisReference, error) {
	refs := make([]OpenAnalysisReference, 0)
	if len(recipeIDs) == 0 {
		return refs, nil
	}
	if err := conn(r.db, ctx).Table("deviation_analyses AS da").
		Select("da.id, da.recipe_id, da.sensor_series_id, ss.run_code AS run_code, da.analysis_state, "+
			"da.initiated_by_name, da.analyzed_at, da.updated_at").
		Joins("LEFT JOIN sensor_series AS ss ON ss.id = da.sensor_series_id").
		Where("da.recipe_id IN ? AND da.analysis_state IN ?", recipeIDs, OpenAnalysisStates).
		Order("da.updated_at DESC, da.id DESC").Scan(&refs).Error; err != nil {
		return nil, fmt.Errorf("list open analysis references: %w", err)
	}
	return refs, nil
}

// LockReferences locks analysis rows by id so a confirm/void cannot move a
// blocker decision in the middle of a release.
func (r *deviationAnalysisRepository) LockReferences(ctx context.Context, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	var lockedIDs []uint
	if err := lockForUpdate(conn(r.db, ctx).Table("deviation_analyses")).
		Where("id IN ?", ids).Pluck("id", &lockedIDs).Error; err != nil {
		return fmt.Errorf("lock deviation analysis references: %w", err)
	}
	return nil
}
