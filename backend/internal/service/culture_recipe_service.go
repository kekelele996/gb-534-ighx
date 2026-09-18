package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/repository"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"
	"gorm.io/gorm"
)

type CultureRecipeService struct {
	recipes repository.CultureRecipeRepository
	vessels repository.FermentationVesselRepository
	series  repository.SensorSeriesRepository
	analyses repository.DeviationAnalysisRepository
	gates   repository.RecipePublishGateRepository
	audits  repository.AuditRepository
	tx      repository.TxRunner
	now     func() time.Time
}

func NewCultureRecipeService(
	recipes repository.CultureRecipeRepository,
	vessels repository.FermentationVesselRepository,
	series repository.SensorSeriesRepository,
	analyses repository.DeviationAnalysisRepository,
	gates repository.RecipePublishGateRepository,
	audits repository.AuditRepository,
	tx repository.TxRunner,
) *CultureRecipeService {
	return &CultureRecipeService{
		recipes: recipes, vessels: vessels, series: series, analyses: analyses, gates: gates,
		audits: audits, tx: tx, now: func() time.Time { return time.Now().UTC() },
	}
}

// PublishBlockedError carries the structured blocker list returned to the
// recipe page when a release is rejected by the version-switch gate.
type PublishBlockedError struct {
	*util.AppError
	Payload dto.PublishBlockedPayload
}

func (e *PublishBlockedError) ErrorDetails() any { return e.Payload }

// Unwrap exposes the embedded AppError so errors.As can classify the 409
// status and code in the unified error handler.
func (e *PublishBlockedError) Unwrap() error { return e.AppError }

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
	response := dto.NewCultureRecipeResponse(recipe)
	if err := s.attachGate(ctx, &response); err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	return response, nil
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
	if err := s.attachGates(ctx, response.Items); err != nil {
		return dto.CultureRecipeListResponse{}, err
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
) (dto.CultureRecipeResponse, error) {
	recipe, err := s.recipes.GetByID(ctx, id, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return dto.CultureRecipeResponse{}, util.NotFound("culture recipe")
		}
		return dto.CultureRecipeResponse{}, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load culture recipe", err)
	}
	from, to := constants.RecipeState(recipe.RecipeState), constants.RecipeState(request.ToState)
	if !constants.CanTransitionRecipe(from, to) {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeStateTransition,
			"illegal recipe transition from "+recipe.RecipeState+" to "+request.ToState)
	}
	if request.Version != recipe.Version {
		return dto.CultureRecipeResponse{}, util.NewError(http.StatusConflict, util.CodeConflict, "recipe version changed concurrently")
	}
	if to == constants.RecipeValidated || to == constants.RecipePublished {
		if err := algorithm.ValidateRecipeConfiguration(
			[]byte(recipe.PhaseBoundariesJSON), []byte(recipe.ReferenceCurvesJSON),
			[]byte(recipe.ToleranceProfileJSON), recipe.TargetDurationH,
		); err != nil {
			return dto.CultureRecipeResponse{}, util.WrapError(http.StatusUnprocessableEntity, util.CodeValidation, "recipe kinetics configuration is invalid", err)
		}
	}
	if to == constants.RecipePublished {
		return s.publish(ctx, recipe, request.Comment, actor)
	}
	if err := s.runGuardedTransition(ctx, recipe, request, actor, from, to); err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	return s.Get(ctx, id)
}

// runGuardedTransition performs every non-publish state move atomically with
// the group gate epoch, so a manual obsolete cannot silently interleave with a
// concurrent release.
func (s *CultureRecipeService) runGuardedTransition(
	ctx context.Context, recipe model.CultureRecipe, request dto.CultureRecipeTransitionRequest,
	actor util.Actor, from, to constants.RecipeState,
) error {
	expectedEpoch, err := s.currentEpoch(ctx, recipe.VesselID, recipe.RecipeCode)
	if err != nil {
		return err
	}
	before := recipe
	return s.tx.InTx(ctx, func(txCtx context.Context) error {
		if err := s.claimGate(txCtx, recipe.VesselID, recipe.RecipeCode, expectedEpoch); err != nil {
			return err
		}
		locked, err := s.recipes.LockByID(txCtx, recipe.ID)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to lock culture recipe", err)
		}
		if locked.Version != recipe.Version || locked.RecipeState != recipe.RecipeState {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
		}
		now := s.now()
		if to == constants.RecipeObsolete {
			changed, transitionErr := s.recipes.ConditionalTransition(txCtx, recipe.ID, string(from), string(to), now)
			if transitionErr != nil {
				return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to obsolete culture recipe", transitionErr)
			}
			if !changed {
				return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state changed concurrently")
			}
		} else {
			changed, transitionErr := s.recipes.Transition(txCtx, recipe.ID, string(from), string(to), recipe.Version, now)
			if transitionErr != nil {
				return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to transition culture recipe", transitionErr)
			}
			if !changed {
				return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state or version changed concurrently")
			}
		}
		recipe.RecipeState = string(to)
		recipe.UpdatedAt = now
		if err := recordAudit(txCtx, s.audits, actor, "culture_recipe", recipe.ID, "transition", before,
			map[string]any{"recipe": recipe, "comment": strings.TrimSpace(request.Comment)}, "", "", 0); err != nil {
			return err
		}
		return nil
	})
}

// publish is the version-switch gate. Inside one database transaction it:
// locks the group gate, re-locks every published predecessor and every live
// reference, rejects the entire release with a blocker list, or atomically
// sets the new version published and all predecessors obsolete. Any failure
// rolls both states back.
func (s *CultureRecipeService) publish(
	ctx context.Context, candidate model.CultureRecipe, comment string, actor util.Actor,
) (dto.CultureRecipeResponse, error) {
	expectedEpoch, err := s.currentEpoch(ctx, candidate.VesselID, candidate.RecipeCode)
	if err != nil {
		return dto.CultureRecipeResponse{}, err
	}
	var response dto.CultureRecipeResponse
	txErr := s.tx.InTx(ctx, func(txCtx context.Context) error {
		now := s.now()
		if err := s.claimGate(txCtx, candidate.VesselID, candidate.RecipeCode, expectedEpoch); err != nil {
			return err
		}
		locked, err := s.recipes.LockByID(txCtx, candidate.ID)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to lock culture recipe", err)
		}
		if locked.Version != candidate.Version {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe version changed concurrently")
		}
		if locked.RecipeState != string(constants.RecipeValidated) {
			return util.NewError(http.StatusConflict, util.CodeStateTransition,
				"recipe can no longer be published from state "+locked.RecipeState)
		}
		predecessors, err := s.recipes.LockPublishedByGroup(txCtx, candidate.VesselID, candidate.RecipeCode)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to lock published recipe versions", err)
		}
		blockers, err := s.collectBlockers(txCtx, predecessors)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			versions := make([]int, 0, len(predecessors))
			for _, predecessor := range predecessors {
				versions = append(versions, predecessor.Version)
			}
			return &PublishBlockedError{
				AppError: util.NewError(http.StatusConflict, util.CodePublishBlocked,
					"recipe release is blocked by live ready series or unconfirmed analyses"),
				Payload: dto.PublishBlockedPayload{
					BlockedRecipeID: candidate.ID, RecipeCode: candidate.RecipeCode,
					ObsoleteVersions: versions, Blockers: blockers,
				},
			}
		}
		// Obsolete predecessors before promoting the candidate: the partial
		// unique index forbids two published versions in a group at commit.
		// Both changes are one atomic unit, so a later failure rolls the
		// predecessors back to published as well.
		for _, predecessor := range predecessors {
			obsoleted, transitionErr := s.recipes.ConditionalTransition(
				txCtx, predecessor.ID, string(constants.RecipePublished), string(constants.RecipeObsolete), now)
			if transitionErr != nil {
				return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to obsolete previous recipe version", transitionErr)
			}
			if !obsoleted {
				return util.NewError(http.StatusConflict, util.CodeConflict, "published recipe version changed concurrently")
			}
		}
		published, err := s.recipes.ConditionalTransition(
			txCtx, candidate.ID, string(constants.RecipeValidated), string(constants.RecipePublished), now)
		if err != nil {
			return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to publish new recipe version", err)
		}
		if !published {
			return util.NewError(http.StatusConflict, util.CodeConflict, "recipe state changed concurrently")
		}
		candidate.RecipeState = string(constants.RecipePublished)
		candidate.UpdatedAt = now
		if err := recordAudit(txCtx, s.audits, actor, "culture_recipe", candidate.ID, "publish", locked,
			map[string]any{"recipe": candidate, "obsoleted_version_ids": recipeIDs(predecessors),
				"comment": strings.TrimSpace(comment)}, "", "", 0); err != nil {
			return err
		}
		for _, predecessor := range predecessors {
			obsolete := predecessor
			obsolete.RecipeState = string(constants.RecipeObsolete)
			obsolete.UpdatedAt = now
			if err := recordAudit(txCtx, s.audits, actor, "culture_recipe", predecessor.ID, "auto_obsolete",
				predecessor, map[string]any{"recipe": obsolete, "replaced_by_recipe_id": candidate.ID,
					"comment": strings.TrimSpace(comment)}, "", "", 0); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		return dto.CultureRecipeResponse{}, txErr
	}
	response, err = s.Get(ctx, idOf(candidate.ID))
	return response, err
}

// collectBlockers locks and inspects every live reference pinned to the
// published predecessors. Locking the referenced rows closes the window in
// which a ready series could be superseded (or an analysis confirmed) while
// the gate decision is in flight.
func (s *CultureRecipeService) collectBlockers(
	ctx context.Context, predecessors []model.CultureRecipe,
) ([]dto.PublishBlocker, error) {
	if len(predecessors) == 0 {
		return []dto.PublishBlocker{}, nil
	}
	ids := make([]uint, 0, len(predecessors))
	for _, predecessor := range predecessors {
		ids = append(ids, predecessor.ID)
	}
	readyRefs, err := s.series.ListReadyReferences(ctx, ids)
	if err != nil {
		return nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to check ready sensor series references", err)
	}
	seriesIDs := make([]uint, 0, len(readyRefs))
	for _, ref := range readyRefs {
		seriesIDs = append(seriesIDs, ref.ID)
	}
	if err := s.series.LockReferences(ctx, seriesIDs); err != nil {
		return nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to lock ready sensor series references", err)
	}
	openRefs, err := s.analyses.ListOpenReferences(ctx, ids)
	if err != nil {
		return nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to check unconfirmed analysis references", err)
	}
	analysisIDs := make([]uint, 0, len(openRefs))
	for _, ref := range openRefs {
		analysisIDs = append(analysisIDs, ref.ID)
	}
	if err := s.analyses.LockReferences(ctx, analysisIDs); err != nil {
		return nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to lock unconfirmed analysis references", err)
	}
	versions := make(map[uint]int, len(predecessors))
	for _, predecessor := range predecessors {
		versions[predecessor.ID] = predecessor.Version
	}
	blockers := make([]dto.PublishBlocker, 0, len(readyRefs)+len(openRefs))
	for _, ref := range readyRefs {
		blockers = append(blockers, dto.PublishBlocker{
			Kind: "ready_series", SeriesID: ref.ID, RecipeID: ref.RecipeID,
			RecipeVersion: versions[ref.RecipeID], RunCode: ref.RunCode, Channel: ref.Channel,
			State: "ready", OccurredAt: ref.UpdatedAt,
			Reason: "ready_sensor_series",
		})
	}
	for _, ref := range openRefs {
		blockers = append(blockers, dto.PublishBlocker{
			Kind: "open_analysis", AnalysisID: ref.ID, RecipeID: ref.RecipeID,
			RecipeVersion: versions[ref.RecipeID], RunCode: ref.RunCode, State: ref.AnalysisState,
			InitiatedByName: ref.InitiatedByName, OccurredAt: ref.UpdatedAt,
			Reason: "unconfirmed_analysis",
		})
	}
	return blockers, nil
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

// currentEpoch reads the group gate version without locking. A first release
// (no gate row yet) starts at epoch 0, which EnsureAndLock materializes.
func (s *CultureRecipeService) currentEpoch(ctx context.Context, vesselID uint, recipeCode string) (int, error) {
	epoch, err := s.gates.Epoch(ctx, vesselID, recipeCode)
	if err != nil {
		return 0, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to read recipe version gate", err)
	}
	return epoch, nil
}

// claimGate materializes and locks the group gate and fails if the epoch
// advanced after the caller read it, i.e. another release or obsolescence won.
func (s *CultureRecipeService) claimGate(ctx context.Context, vesselID uint, recipeCode string, expectedEpoch int) error {
	gate, err := s.gates.EnsureAndLock(ctx, vesselID, recipeCode, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrLockContention) {
			return util.NewError(http.StatusConflict, util.CodeConflict,
				"a concurrent recipe publish or obsolescence is in progress; refresh and retry")
		}
		return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to acquire recipe version gate", err)
	}
	if gate.Epoch != expectedEpoch {
		return util.NewError(http.StatusConflict, util.CodeConflict,
			"a concurrent recipe publish or obsolescence completed first; refresh and retry")
	}
	bumped, err := s.gates.BumpEpoch(ctx, vesselID, recipeCode, expectedEpoch, s.now())
	if err != nil {
		if errors.Is(err, repository.ErrLockContention) {
			return util.NewError(http.StatusConflict, util.CodeConflict,
				"a concurrent recipe publish or obsolescence is in progress; refresh and retry")
		}
		return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to advance recipe version gate", err)
	}
	if !bumped {
		return util.NewError(http.StatusConflict, util.CodeConflict,
			"a concurrent recipe publish or obsolescence completed first; refresh and retry")
	}
	return nil
}

func recipeIDs(recipes []model.CultureRecipe) []uint {
	ids := make([]uint, 0, len(recipes))
	for _, recipe := range recipes {
		ids = append(ids, recipe.ID)
	}
	return ids
}

// attachGate projects the version-switch gate for one recipe. A validated
// version previews what would block its release; a published version lists
// the live references that currently prevent forced obsolescence.
func (s *CultureRecipeService) attachGate(ctx context.Context, response *dto.CultureRecipeResponse) error {
	switch constants.RecipeState(response.RecipeState) {
	case constants.RecipeValidated, constants.RecipePublished:
	default:
		return nil
	}
	group, err := s.recipes.ListPublishedVersions(ctx, response.VesselID, response.RecipeCode)
	if err != nil {
		return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load published recipe versions", err)
	}
	ids := recipeIDs(group)
	if response.RecipeState == string(constants.RecipeValidated) && len(ids) == 0 {
		response.PublishGate = &dto.PublishGate{Blocked: false, Blockers: []dto.PublishBlocker{}}
		return nil
	}
	blockers, err := s.referenceBlockers(ctx, group)
	if err != nil {
		return err
	}
	response.PublishGate = &dto.PublishGate{
		Blocked: response.RecipeState == string(constants.RecipeValidated) && len(blockers) > 0,
		Blockers: blockers,
	}
	return nil
}

// attachGates batches the reference queries for a recipe list page so the
// gate projection costs two queries regardless of the row count.
func (s *CultureRecipeService) attachGates(ctx context.Context, items []dto.CultureRecipeResponse) error {
	type target struct {
		index    int
		groupIDs []uint
		versions map[uint]int
	}
	targets := make([]target, 0)
	groupKey := func(vesselID uint, code string) string {
		return strconv.FormatUint(uint64(vesselID), 10) + "/" + code
	}
	// Collect published siblings per vessel+code group first.
	publishedByGroup := map[string][]model.CultureRecipe{}
	for i := range items {
		if constants.RecipeState(items[i].RecipeState) != constants.RecipeValidated &&
			constants.RecipeState(items[i].RecipeState) != constants.RecipePublished {
			continue
		}
		key := groupKey(items[i].VesselID, items[i].RecipeCode)
		if _, ok := publishedByGroup[key]; !ok {
			group, err := s.recipes.ListPublishedVersions(ctx, items[i].VesselID, items[i].RecipeCode)
			if err != nil {
				return util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to load published recipe versions", err)
			}
			publishedByGroup[key] = group
		}
		group := publishedByGroup[key]
		versions := make(map[uint]int, len(group))
		ids := make([]uint, 0, len(group))
		for _, recipe := range group {
			versions[recipe.ID] = recipe.Version
			ids = append(ids, recipe.ID)
		}
		targets = append(targets, target{index: i, groupIDs: ids, versions: versions})
	}
	allIDs := map[uint]struct{}{}
	for _, target := range targets {
		for _, id := range target.groupIDs {
			allIDs[id] = struct{}{}
		}
	}
	readyByRecipe, openByRecipe, err := s.referenceMaps(ctx, uintSet(allIDs))
	if err != nil {
		return err
	}
	for _, target := range targets {
		blockers := make([]dto.PublishBlocker, 0)
		for _, id := range target.groupIDs {
			blockers = append(blockers, readyByRecipe[id]...)
			blockers = append(blockers, openByRecipe[id]...)
		}
		for i := range blockers {
			if version, ok := target.versions[blockers[i].RecipeID]; ok {
				blockers[i].RecipeVersion = version
			}
		}
		items[target.index].PublishGate = &dto.PublishGate{
			Blocked: items[target.index].RecipeState == string(constants.RecipeValidated) && len(blockers) > 0,
			Blockers: blockers,
		}
	}
	return nil
}

func (s *CultureRecipeService) referenceBlockers(
	ctx context.Context, published []model.CultureRecipe,
) ([]dto.PublishBlocker, error) {
	readyByRecipe, openByRecipe, err := s.referenceMaps(ctx, recipeIDs(published))
	if err != nil {
		return nil, err
	}
	versions := make(map[uint]int, len(published))
	for _, recipe := range published {
		versions[recipe.ID] = recipe.Version
	}
	blockers := make([]dto.PublishBlocker, 0)
	for _, recipe := range published {
		for _, blocker := range readyByRecipe[recipe.ID] {
			blocker.RecipeVersion = versions[blocker.RecipeID]
			blockers = append(blockers, blocker)
		}
		for _, blocker := range openByRecipe[recipe.ID] {
			blocker.RecipeVersion = versions[blocker.RecipeID]
			blockers = append(blockers, blocker)
		}
	}
	return blockers, nil
}

func (s *CultureRecipeService) referenceMaps(
	ctx context.Context, recipeIDs []uint,
) (map[uint][]dto.PublishBlocker, map[uint][]dto.PublishBlocker, error) {
	readyByRecipe := map[uint][]dto.PublishBlocker{}
	openByRecipe := map[uint][]dto.PublishBlocker{}
	if len(recipeIDs) == 0 {
		return readyByRecipe, openByRecipe, nil
	}
	readyRefs, err := s.series.ListReadyReferences(ctx, recipeIDs)
	if err != nil {
		return nil, nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to read ready sensor series references", err)
	}
	for _, ref := range readyRefs {
		readyByRecipe[ref.RecipeID] = append(readyByRecipe[ref.RecipeID], dto.PublishBlocker{
			Kind: "ready_series", SeriesID: ref.ID, RecipeID: ref.RecipeID, RunCode: ref.RunCode,
			Channel: ref.Channel, State: "ready", OccurredAt: ref.UpdatedAt, Reason: "ready_sensor_series",
		})
	}
	openRefs, err := s.analyses.ListOpenReferences(ctx, recipeIDs)
	if err != nil {
		return nil, nil, util.WrapError(http.StatusInternalServerError, util.CodeInternal, "unable to read unconfirmed analysis references", err)
	}
	for _, ref := range openRefs {
		openByRecipe[ref.RecipeID] = append(openByRecipe[ref.RecipeID], dto.PublishBlocker{
			Kind: "open_analysis", AnalysisID: ref.ID, RecipeID: ref.RecipeID, RunCode: ref.RunCode,
			State: ref.AnalysisState, InitiatedByName: ref.InitiatedByName,
			OccurredAt: ref.UpdatedAt, Reason: "unconfirmed_analysis",
		})
	}
	return readyByRecipe, openByRecipe, nil
}

func uintSet(values map[uint]struct{}) []uint {
	ids := make([]uint, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	return ids
}

func idOf(id uint) uint { return id }
