package service

import (
	"context"
	"errors"
	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/repository"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"
	"fmt"
	"gorm.io/gorm"
	"net/http"
	"strings"
	"time"
)

type CultureRecipeService struct {
	recipes      repository.CultureRecipeRepository
	vessels      repository.FermentationVesselRepository
	series       repository.SensorSeriesRepository
	analyses     repository.DeviationAnalysisRepository
	audits       repository.AuditRepository
	transactions repository.TransactionManager
	switchGate   *util.SwitchGate
	now          func() time.Time
	// onSwitchHeld, when set, runs while a request holds the group switch slot
	// and before the database transaction starts. It exists to make concurrent
	// switch tests deterministic and is always nil in production wiring.
	onSwitchHeld func(key string)
}

func NewCultureRecipeService(
	recipes repository.CultureRecipeRepository,
	vessels repository.FermentationVesselRepository,
	series repository.SensorSeriesRepository,
	analyses repository.DeviationAnalysisRepository,
	audits repository.AuditRepository,
	transactions repository.TransactionManager,
) *CultureRecipeService {
	return &CultureRecipeService{
		recipes: recipes, vessels: vessels, series: series, analyses: analyses, audits: audits,
		transactions: transactions, switchGate: util.NewSwitchGate(),
		now: func() time.Time { return time.Now().UTC() },
	}
}
func (s *CultureRecipeService) Create(
	ctx context.Context, request dto.CreateCultureRecipeRequest, actor util.Actor,
) (dto.CultureRecipeResponse, error) {
	request.Normalize()
	vessel, err := s.vessels.GetByID(ctx, request.VesselID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("fermentation vessel")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load fermentation vessel", err)
	}
	if !vessel.Active() {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeStateTransition, "cannot create a recipe for an inactive vessel")
	}
	if err := algorithm.ValidateRecipeConfiguration(
		request.PhaseBoundariesJSON, request.ReferenceCurvesJSON, request.ToleranceProfileJSON, request.TargetDurationH,
	); err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusUnprocessableEntity, util.CodeValidation, "recipe kinetics configuration is invalid", err)
	}
	maxVersion, err := s.recipes.MaxVersion(ctx, request.VesselID, request.RecipeCode)
	if err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to check recipe version", err)
	}
	if maxVersion != 0 {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe code already exists; copy its version instead")
	}
	now := s.now()
	recipe := model.CultureRecipe{
		VesselID: request.VesselID, RecipeCode: request.RecipeCode, Version: 1, Organism: request.Organism,
		TargetDurationH: request.TargetDurationH, PhaseBoundariesJSON: string(request.PhaseBoundariesJSON),
		ReferenceCurvesJSON: string(request.ReferenceCurvesJSON), ToleranceProfileJSON: string(request.ToleranceProfileJSON),
		RecipeState: string(constants.RecipeDraft), CreatedBy: actor.UserID, CreatedByName: actor.Username,
		CreatedAt: now, UpdatedAt: now, Vessel: vessel,
	}
	if err := s.recipes.Create(ctx, &recipe); err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusConflict, util.CodeConflict, "recipe code and version must be unique", err)
	}
	if err := recordAudit(ctx, s.audits, actor, "culture_recipe", recipe.ID, "create", nil, recipe, "", "", 0); err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	return dto.NewCultureRecipeResponse(recipe), nil
}
func (s *CultureRecipeService) Get(ctx context.Context, id uint) (dto.CultureRecipeResponse, error) {
	recipe, err := s.recipes.GetByID(ctx, id, true)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("culture recipe")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load culture recipe", err)
	}
	return dto.NewCultureRecipeResponse(recipe), nil
}
func (s *CultureRecipeService) List(ctx context.Context, query dto.CultureRecipeQuery) (dto.CultureRecipeListResponse, error) {
	recipes, total, err := s.recipes.List(ctx, query)
	if err != nil {
		return dto.CultureRecipeListResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to list culture recipes", err)
	}
	response := dto.CultureRecipeListResponse{
		Items: make([]dto.CultureRecipeResponse, 0, len(recipes)), Total: total, Page: query.Page, Size: query.PageSize,
	}
	for _, recipe := range recipes {
		response.Items = append(response.Items, dto.NewCultureRecipeResponse(recipe))
	}
	return response, nil
}
func (s *CultureRecipeService) Update(
	ctx context.Context, id uint, request dto.UpdateCultureRecipeRequest, actor util.Actor,
) (dto.CultureRecipeResponse, error) {
	request.Normalize()
	recipe, err := s.recipes.GetByID(ctx, id, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("culture recipe")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load culture recipe", err)
	}
	if !recipe.Editable() {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeStateTransition, "published or obsolete recipe versions cannot be edited")
	}
	if recipe.Version != request.Version {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe version changed concurrently")
	}
	before := recipe
	if request.Organism != nil {
		recipe.Organism = *request.Organism
	}
	if request.TargetDurationH != nil {
		recipe.TargetDurationH = *request.TargetDurationH
	}
	if request.PhaseBoundariesJSON != nil {
		recipe.PhaseBoundariesJSON = string(*request.PhaseBoundariesJSON)
	}
	if request.ReferenceCurvesJSON != nil {
		recipe.ReferenceCurvesJSON = string(*request.ReferenceCurvesJSON)
	}
	if request.ToleranceProfileJSON != nil {
		recipe.ToleranceProfileJSON = string(*request.ToleranceProfileJSON)
	}
	if err := algorithm.ValidateRecipeConfiguration(
		[]byte(recipe.PhaseBoundariesJSON), []byte(recipe.ReferenceCurvesJSON),
		[]byte(recipe.ToleranceProfileJSON), recipe.TargetDurationH,
	); err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusUnprocessableEntity, util.CodeValidation, "recipe kinetics configuration is invalid", err)
	}
	recipe.UpdatedAt = s.now()
	changed, err := s.recipes.UpdateWithVersion(ctx, &recipe, request.Version)
	if err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to update culture recipe", err)
	}
	if !changed {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
	}
	if err := recordAudit(ctx, s.audits, actor, "culture_recipe", recipe.ID, "update", before, recipe, "", "", 0); err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	return s.Get(ctx, recipe.ID)
}
func (s *CultureRecipeService) Transition(
	ctx context.Context, id uint, request dto.CultureRecipeTransitionRequest, actor util.Actor,
) (dto.CultureRecipeTransitionResult, error) {
	recipe, err := s.recipes.GetByID(ctx, id, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeTransitionResult{}, util.NotFound("culture recipe")
		}
		return dto.CultureRecipeTransitionResult{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load culture recipe", err)
	}
	from, to := constants.RecipeState(recipe.RecipeState), constants.RecipeState(request.ToState)
	if !constants.CanTransitionRecipe(from, to) {
		return dto.CultureRecipeTransitionResult{}, util.NewError(http.StatusConflict, util.CodeStateTransition,
			"illegal recipe transition from "+recipe.RecipeState+" to "+request.ToState)
	}
	if request.Version != recipe.Version {
		return dto.CultureRecipeTransitionResult{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe version changed concurrently")
	}
	switch to {
	case constants.RecipePublished:
		return s.publishWithGate(ctx, recipe, request, actor)
	case constants.RecipeObsolete:
		return s.obsoletePublished(ctx, recipe, request, actor)
	default:
		return s.simpleTransition(ctx, recipe, request, actor)
	}
}

// GatePreview evaluates the version switch gate for a recipe without changing
// any state. It lets the recipes page re-read the blocking list after a refresh
// rather than relying solely on the failed publish response.
func (s *CultureRecipeService) GatePreview(ctx context.Context, id uint) (dto.RecipePublishGate, error) {
	recipe, err := s.recipes.GetByID(ctx, id, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.RecipePublishGate{}, util.NotFound("culture recipe")
		}
		return dto.RecipePublishGate{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load culture recipe", err)
	}
	if !constants.CanTransitionRecipe(constants.RecipeState(recipe.RecipeState), constants.RecipePublished) {
		return dto.RecipePublishGate{}, util.NewError(http.StatusConflict, util.CodeStateTransition,
			"only validated recipe versions can be published")
	}
	oldPublished, err := s.recipes.ListPublishedInGroup(ctx, recipe.VesselID, recipe.RecipeCode, recipe.ID)
	if err != nil {
		return dto.RecipePublishGate{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load published recipe versions", err)
	}
	blockers, readyCount, openCount, err := s.collectBlockers(
		ctx, blockerPair{s.series, s.analyses}, recipe, oldPublished,
	)
	if err != nil {
		return dto.RecipePublishGate{}, err
	}
	publishedResponses := make([]dto.CultureRecipeResponse, 0, len(oldPublished))
	for _, old := range oldPublished {
		publishedResponses = append(publishedResponses, dto.NewCultureRecipeResponse(old))
	}
	return dto.RecipePublishGate{
		RecipeCode: recipe.RecipeCode, VesselID: recipe.VesselID, TargetVersion: recipe.Version,
		PublishedVersions: publishedResponses, Blockers: blockers,
		ReadySeriesCount: readyCount, OpenAnalysisCount: openCount,
	}, nil
}

// simpleTransition handles draft <-> validated moves, which never supersede
// other versions and therefore do not need the publish-group gate.
func (s *CultureRecipeService) simpleTransition(
	ctx context.Context, recipe model.CultureRecipe, request dto.CultureRecipeTransitionRequest, actor util.Actor,
) (dto.CultureRecipeTransitionResult, error) {
	if err := algorithm.ValidateRecipeConfiguration(
		[]byte(recipe.PhaseBoundariesJSON), []byte(recipe.ReferenceCurvesJSON),
		[]byte(recipe.ToleranceProfileJSON), recipe.TargetDurationH,
	); err != nil {
		return dto.CultureRecipeTransitionResult{}, util.WrapError(http.StatusUnprocessableEntity, util.CodeValidation, "recipe kinetics configuration is invalid", err)
	}
	before := recipe
	now := s.now()
	changed, err := s.recipes.Transition(ctx, recipe.ID, recipe.RecipeState, request.ToState, request.Version, now)
	if err != nil {
		return dto.CultureRecipeTransitionResult{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to transition culture recipe", err)
	}
	if !changed {
		return dto.CultureRecipeTransitionResult{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
	}
	recipe.RecipeState = request.ToState
	recipe.UpdatedAt = now
	if err := recordAudit(ctx, s.audits, actor, "culture_recipe", recipe.ID, "transition", before,
		map[string]any{"recipe": recipe, "comment": strings.TrimSpace(request.Comment)}, "", "", 0); err != nil {
		return dto.CultureRecipeTransitionResult{}, err
	}
	latest, err := s.Get(ctx, recipe.ID)
	if err != nil {
		return dto.CultureRecipeTransitionResult{}, err
	}
	return dto.CultureRecipeTransitionResult{Recipe: latest}, nil
}

// publishWithGate serializes all publish/obsolete switches for one vessel +
// recipe code, checks references against the previously published versions
// inside a transaction, and applies the new published state together with the
// auto-obsoletion of the old versions. Any failure rolls every step back.
func (s *CultureRecipeService) publishWithGate(
	ctx context.Context, recipe model.CultureRecipe, request dto.CultureRecipeTransitionRequest, actor util.Actor,
) (dto.CultureRecipeTransitionResult, error) {
	if err := algorithm.ValidateRecipeConfiguration(
		[]byte(recipe.PhaseBoundariesJSON), []byte(recipe.ReferenceCurvesJSON),
		[]byte(recipe.ToleranceProfileJSON), recipe.TargetDurationH,
	); err != nil {
		return dto.CultureRecipeTransitionResult{}, util.WrapError(http.StatusUnprocessableEntity, util.CodeValidation, "recipe kinetics configuration is invalid", err)
	}
	key := recipeGroupKey(recipe.VesselID, recipe.RecipeCode)
	// Register, snapshot and acquire the group slot. A competing switch that
	// commits while we queue advances the sequence, so this publish refuses
	// rather than racing the winner.
	observedSeq := s.switchGate.Enter(key)
	defer s.switchGate.Leave(key)
	if s.switchGate.Sequence(key) != observedSeq {
		return dto.CultureRecipeTransitionResult{}, util.NewError(http.StatusConflict, util.CodeConflict,
			"a concurrent publish or obsolete was accepted first; refresh and retry")
	}
	s.gateHeld(key)

	var (
		result     dto.CultureRecipeTransitionResult
		blockedErr *util.AppError
	)
	txErr := s.transactions.WithinTransaction(ctx, func(tx repository.TxRepositories) error {
		// Re-read inside the transaction so a state that changed while waiting
		// for the group lock cannot be published blindly.
		current, err := tx.Recipes.GetByID(ctx, recipe.ID, false)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to re-load culture recipe", err)
		}
		if current.Version != request.Version {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe version changed concurrently")
		}
		if current.RecipeState != recipe.RecipeState {
			if !constants.CanTransitionRecipe(constants.RecipeState(current.RecipeState), constants.RecipePublished) {
				return util.NewError(http.StatusConflict, util.CodeStateTransition,
					"recipe state changed concurrently; only validated versions can be published")
			}
		}
		oldPublished, err := tx.Recipes.ListPublishedInGroup(ctx, recipe.VesselID, recipe.RecipeCode, recipe.ID)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load published recipe versions", err)
		}
		blockers, readyCount, openCount, err := s.collectBlockers(ctx, blockerPair{tx.Series, tx.Analyses}, recipe, oldPublished)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			publishedResponses := make([]dto.CultureRecipeResponse, 0, len(oldPublished))
			for _, old := range oldPublished {
				publishedResponses = append(publishedResponses, dto.NewCultureRecipeResponse(old))
			}
			blockedErr = util.NewError(http.StatusConflict, util.CodePublishBlocked,
				"recipe publish is blocked by ready sensor series or unconfirmed analyses").
				WithDetails(dto.RecipePublishGate{
					RecipeCode: recipe.RecipeCode, VesselID: recipe.VesselID, TargetVersion: recipe.Version,
					PublishedVersions: publishedResponses, Blockers: blockers,
					ReadySeriesCount: readyCount, OpenAnalysisCount: openCount,
				})
			return errPublishRejected
		}
		now := s.now()
		oldIDs := make([]uint, 0, len(oldPublished))
		obsoletedResponses := make([]dto.CultureRecipeResponse, 0, len(oldPublished))
		for _, old := range oldPublished {
			oldIDs = append(oldIDs, old.ID)
			response := dto.NewCultureRecipeResponse(old)
			response.RecipeState = string(constants.RecipeObsolete)
			response.UpdatedAt = now
			obsoletedResponses = append(obsoletedResponses, response)
		}
		// Retire the previously published versions first so the partial unique
		// index never sees two published rows in the group; the new version is
		// published in the same transaction immediately afterwards, so the
		// switch is still atomic from the client's point of view.
		obsoletedCount, err := tx.Recipes.BulkObsolete(ctx, oldIDs, now)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to obsolete previous recipe versions", err)
		}
		if obsoletedCount != int64(len(oldIDs)) {
			return util.NewError(http.StatusConflict, util.CodeConflict, "published recipe versions changed concurrently")
		}
		changed, err := tx.Recipes.Transition(ctx, recipe.ID, current.RecipeState, request.ToState, request.Version, now)
		if err != nil {
			return util.WrapError(http.StatusConflict, util.CodeConflict, "unable to publish culture recipe", err)
		}
		if !changed {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
		}
		publishedRecipe := current
		publishedRecipe.RecipeState = request.ToState
		publishedRecipe.UpdatedAt = now
		comment := strings.TrimSpace(request.Comment)
		if err := recordAudit(ctx, tx.Audits, actor, "culture_recipe", recipe.ID, "publish",
			current, map[string]any{"recipe": publishedRecipe, "obsoleted_version_ids": oldIDs, "comment": comment},
			"", "", 0); err != nil {
			return err
		}
		for _, old := range oldPublished {
			before := old
			after := old
			after.RecipeState = string(constants.RecipeObsolete)
			after.UpdatedAt = now
			if err := recordAudit(ctx, tx.Audits, actor, "culture_recipe", old.ID, "auto_obsolete",
				before, map[string]any{"recipe": after, "superseded_by_recipe_id": recipe.ID, "comment": comment},
				"", "", 0); err != nil {
				return err
			}
		}
		result = dto.CultureRecipeTransitionResult{ObsoletedVersions: obsoletedResponses}
		return nil
	})
	if blockedErr != nil {
		return dto.CultureRecipeTransitionResult{}, blockedErr
	}
	if txErr != nil {
		return dto.CultureRecipeTransitionResult{}, txErr
	}
	s.switchGate.Bump(key)
	latest, err := s.Get(ctx, recipe.ID)
	if err != nil {
		return dto.CultureRecipeTransitionResult{}, err
	}
	result.Recipe = latest
	return result, nil
}

// obsoletePublished handles a manual published -> obsolete switch and shares
// the same per-group lock and switch sequence as a publish, so concurrently
// issued publish/obsolete requests cannot both be accepted.
func (s *CultureRecipeService) obsoletePublished(
	ctx context.Context, recipe model.CultureRecipe, request dto.CultureRecipeTransitionRequest, actor util.Actor,
) (dto.CultureRecipeTransitionResult, error) {
	key := recipeGroupKey(recipe.VesselID, recipe.RecipeCode)
	observedSeq := s.switchGate.Enter(key)
	defer s.switchGate.Leave(key)
	if s.switchGate.Sequence(key) != observedSeq {
		return dto.CultureRecipeTransitionResult{}, util.NewError(http.StatusConflict, util.CodeConflict,
			"a concurrent publish or obsolete was accepted first; refresh and retry")
	}
	s.gateHeld(key)
	var result dto.CultureRecipeTransitionResult
	txErr := s.transactions.WithinTransaction(ctx, func(tx repository.TxRepositories) error {
		now := s.now()
		changed, err := tx.Recipes.Transition(ctx, recipe.ID, recipe.RecipeState, request.ToState, request.Version, now)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to obsolete culture recipe", err)
		}
		if !changed {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
		}
		after := recipe
		after.RecipeState = request.ToState
		after.UpdatedAt = now
		if err := recordAudit(ctx, tx.Audits, actor, "culture_recipe", recipe.ID, "transition",
			recipe, map[string]any{"recipe": after, "comment": strings.TrimSpace(request.Comment)}, "", "", 0); err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return dto.CultureRecipeTransitionResult{}, txErr
	}
	s.switchGate.Bump(key)
	latest, err := s.Get(ctx, recipe.ID)
	if err != nil {
		return dto.CultureRecipeTransitionResult{}, err
	}
	result.Recipe = latest
	return result, nil
}

// errPublishRejected is a sentinel that aborts and rolls back the transaction
// when blockers were found; the structured gate error is returned to the caller
// instead of being surfaced as an internal failure.
var errPublishRejected = errors.New("recipe publish rejected by version switch gate")

// blockerSources is the subset of repositories needed to evaluate a publish
// gate; both the transaction bundle and the ordinary repositories satisfy it.
type blockerSources interface {
	ListReadyByRecipeIDs(context.Context, []uint) ([]model.SensorSeries, error)
	ListOpenByRecipeIDs(context.Context, []uint, []string) ([]model.DeviationAnalysis, error)
}

// blockerPair adapts the concrete series and analysis repositories to
// blockerSources, for read-only gate evaluation outside a transaction.
type blockerPair struct {
	series   repository.SensorSeriesRepository
	analyses repository.DeviationAnalysisRepository
}

func (p blockerPair) ListReadyByRecipeIDs(ctx context.Context, ids []uint) ([]model.SensorSeries, error) {
	return p.series.ListReadyByRecipeIDs(ctx, ids)
}

func (p blockerPair) ListOpenByRecipeIDs(ctx context.Context, ids []uint, states []string) ([]model.DeviationAnalysis, error) {
	return p.analyses.ListOpenByRecipeIDs(ctx, ids, states)
}

// collectBlockers gathers ready sensor series and unconfirmed analyses that
// reference a currently published version the publish would obsolete.
func (s *CultureRecipeService) collectBlockers(
	ctx context.Context, source blockerSources, target model.CultureRecipe, oldPublished []model.CultureRecipe,
) ([]dto.RecipeBlocker, int, int, error) {
	if len(oldPublished) == 0 {
		return nil, 0, 0, nil
	}
	oldIDs := make([]uint, 0, len(oldPublished))
	versionByID := make(map[uint]int, len(oldPublished))
	for _, old := range oldPublished {
		oldIDs = append(oldIDs, old.ID)
		versionByID[old.ID] = old.Version
	}
	readySeries, err := source.ListReadyByRecipeIDs(ctx, oldIDs)
	if err != nil {
		return nil, 0, 0, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to check ready sensor series", err)
	}
	openAnalyses, err := source.ListOpenByRecipeIDs(ctx, oldIDs, constants.RecipeBlockingAnalysisStates())
	if err != nil {
		return nil, 0, 0, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to check unconfirmed analyses", err)
	}
	blockers := make([]dto.RecipeBlocker, 0, len(readySeries)+len(openAnalyses))
	for _, item := range readySeries {
		started := item.StartedAt
		blockers = append(blockers, dto.RecipeBlocker{
			Kind: "ready_series", ID: item.ID, State: item.SeriesState,
			Label: item.RunCode + " · " + item.Channel, VesselID: item.VesselID,
			RecipeID: item.RecipeID, RecipeCode: target.RecipeCode, RecipeVer: versionByID[item.RecipeID],
			SeriesRunCode: item.RunCode, Channel: item.Channel, StartedAt: &started,
			Reason: "ready sensor series still references this published recipe version",
		})
	}
	for _, item := range openAnalyses {
		analyzed := item.AnalyzedAt
		blockers = append(blockers, dto.RecipeBlocker{
			Kind: "open_analysis", ID: item.ID, State: item.AnalysisState,
			Label: fmt.Sprintf("analysis #%d · %s", item.ID, item.AnalysisState), VesselID: target.VesselID,
			RecipeID: item.RecipeID, RecipeCode: target.RecipeCode,
			RecipeVer: item.RecipeVersion, AnalyzedAt: &analyzed, Initiator: item.InitiatedByName,
			Reason: "unconfirmed deviation analysis still references this published recipe version",
		})
	}
	return blockers, len(readySeries), len(openAnalyses), nil
}
func recipeGroupKey(vesselID uint, recipeCode string) string {
	return fmt.Sprintf("%d/%s", vesselID, recipeCode)
}

// gateHeld invokes the optional test synchronization hook while the caller
// holds the exclusive group switch slot.
func (s *CultureRecipeService) gateHeld(key string) {
	if s.onSwitchHeld != nil {
		s.onSwitchHeld(key)
	}
}
func (s *CultureRecipeService) Copy(
	ctx context.Context, id uint, request dto.CopyCultureRecipeRequest, actor util.Actor,
) (dto.CultureRecipeResponse, error) {
	source, err := s.recipes.GetByID(ctx, id, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("culture recipe")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load source recipe", err)
	}
	targetVesselID := request.VesselID
	if targetVesselID == 0 {
		targetVesselID = source.VesselID
	}
	vessel, err := s.vessels.GetByID(ctx, targetVesselID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("target fermentation vessel")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load target vessel", err)
	}
	if !vessel.Active() {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeStateTransition, "cannot copy a recipe to an inactive vessel")
	}
	version, err := s.recipes.MaxVersion(ctx, targetVesselID, source.RecipeCode)
	if err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to allocate recipe version", err)
	}
	now := s.now()
	copyRecipe := model.CultureRecipe{
		VesselID: targetVesselID, Vessel: vessel, RecipeCode: source.RecipeCode, Version: version + 1,
		Organism: source.Organism, TargetDurationH: source.TargetDurationH,
		PhaseBoundariesJSON: source.PhaseBoundariesJSON, ReferenceCurvesJSON: source.ReferenceCurvesJSON,
		ToleranceProfileJSON: source.ToleranceProfileJSON, RecipeState: string(constants.RecipeDraft),
		CreatedBy: actor.UserID, CreatedByName: actor.Username, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.recipes.Create(ctx, &copyRecipe); err != nil {
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusConflict, util.CodeConflict, "recipe version was allocated concurrently", err)
	}
	if err := recordAudit(ctx, s.audits, actor, "culture_recipe", copyRecipe.ID, "copy_version", source,
		map[string]any{"recipe": copyRecipe, "comment": strings.TrimSpace(request.Comment)}, "", "", 0); err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	return dto.NewCultureRecipeResponse(copyRecipe), nil
}
