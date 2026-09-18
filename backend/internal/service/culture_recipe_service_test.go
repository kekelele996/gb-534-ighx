package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/repository"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type recipeGateFixture struct {
	db       *gorm.DB
	svc      *CultureRecipeService
	vessel   model.FermentationVessel
	recipeRepo repository.CultureRecipeRepository
	seriesRepo repository.SensorSeriesRepository
	analysisRepo repository.DeviationAnalysisRepository
	auditRepo repository.AuditRepository
	actor    util.Actor
}

func setupRecipeGateFixture(t *testing.T) recipeGateFixture {
	t.Helper()
	db := newTestDB(t)
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	gateRepo := repository.NewRecipePublishGateRepository(db)
	vessel := model.FermentationVessel{
		VesselCode: "FV-T1", Name: "Test vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	svc := NewCultureRecipeService(
		recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, repository.NewTxRunner(db),
	)
	return recipeGateFixture{
		db: db, svc: svc, vessel: vessel, recipeRepo: recipeRepo,
		seriesRepo: seriesRepo, analysisRepo: analysisRepo, auditRepo: auditRepo,
		actor: util.Actor{UserID: 1, Username: "scientist", Role: string(constants.RoleProcessScientist), RequestID: "req-test"},
	}
}

func createRecipeVersion(t *testing.T, fixture recipeGateFixture, version int, state constants.RecipeState) model.CultureRecipe {
	t.Helper()
	boundaries, references, tolerances := testRecipeConfig(t)
	now := time.Now().UTC()
	recipe := model.CultureRecipe{
		VesselID: fixture.vessel.ID, RecipeCode: "GATE-A", Version: version, Organism: "Test organism",
		TargetDurationH: 8, PhaseBoundariesJSON: string(boundaries), ReferenceCurvesJSON: string(references),
		ToleranceProfileJSON: string(tolerances), RecipeState: string(state),
		CreatedBy: fixture.actor.UserID, CreatedByName: fixture.actor.Username, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.recipeRepo.Create(context.Background(), &recipe); err != nil {
		t.Fatalf("create recipe v%d: %v", version, err)
	}
	return recipe
}

func transitionRecipe(t *testing.T, fixture recipeGateFixture, id uint, to constants.RecipeState, version int) {
	t.Helper()
	if _, err := fixture.svc.Transition(context.Background(), id, dto.CultureRecipeTransitionRequest{
		ToState: string(to), Version: version,
	}, fixture.actor); err != nil {
		t.Fatalf("transition recipe %d to %s: %v", id, to, err)
	}
}

func TestCultureRecipeLifecycleAndVersionCopy(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	boundaries, references, tolerances := testRecipeConfig(t)
	created, err := fixture.svc.Create(context.Background(), dto.CreateCultureRecipeRequest{
		VesselID: fixture.vessel.ID, RecipeCode: "TEST-A", Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: boundaries, ReferenceCurvesJSON: references, ToleranceProfileJSON: tolerances,
	}, fixture.actor)
	if err != nil {
		t.Fatalf("create recipe: %v", err)
	}
	transitionRecipe(t, fixture, created.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, created.ID, constants.RecipePublished, 1)
	copied, err := fixture.svc.Copy(context.Background(), created.ID, dto.CopyCultureRecipeRequest{}, fixture.actor)
	if err != nil {
		t.Fatalf("copy recipe: %v", err)
	}
	if copied.Version != 2 || copied.RecipeState != "draft" {
		t.Fatalf("copy version=%d state=%s, want 2 draft", copied.Version, copied.RecipeState)
	}
}

func TestPublishAutoObsoletesPreviousVersionAtomically(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
	transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
	v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
	transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)
	transitionRecipe(t, fixture, v2.ID, constants.RecipePublished, 2)

	updated1, err := fixture.recipeRepo.GetByID(context.Background(), v1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	updated2, err := fixture.recipeRepo.GetByID(context.Background(), v2.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated1.RecipeState != "obsolete" {
		t.Fatalf("v1 state=%s, want obsolete", updated1.RecipeState)
	}
	if updated2.RecipeState != "published" {
		t.Fatalf("v2 state=%s, want published", updated2.RecipeState)
	}
	response, err := fixture.svc.Get(context.Background(), v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response.PublishGate == nil || response.PublishGate.Blocked {
		t.Fatalf("published v2 gate=%+v, want non-blocked gate", response.PublishGate)
	}
}

func TestPublishBlockedByReadySeriesRollsBothStatesBack(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
	transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
	series := createReadySeries(t, fixture, v1)
	t.Cleanup(func() { _ = series.ID })
	v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
	transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)

	_, err := fixture.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 2,
	}, fixture.actor)
	var blocked *PublishBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("want PublishBlockedError, got %v", err)
	}
	if blocked.Payload.BlockedRecipeID != v2.ID {
		t.Fatalf("blocked recipe id=%d, want %d", blocked.Payload.BlockedRecipeID, v2.ID)
	}
	if len(blocked.Payload.Blockers) != 1 || blocked.Payload.Blockers[0].Kind != "ready_series" {
		t.Fatalf("blockers=%+v, want one ready_series blocker", blocked.Payload.Blockers)
	}
	if blocked.Payload.Blockers[0].RunCode != "RUN-READY-1" {
		t.Fatalf("blocker run code=%s, want RUN-READY-1", blocked.Payload.Blockers[0].RunCode)
	}
	updated1, _ := fixture.recipeRepo.GetByID(context.Background(), v1.ID, false)
	updated2, _ := fixture.recipeRepo.GetByID(context.Background(), v2.ID, false)
	if updated1.RecipeState != "published" {
		t.Fatalf("v1 state=%s, want published after rollback", updated1.RecipeState)
	}
	if updated2.RecipeState != "validated" {
		t.Fatalf("v2 state=%s, want validated after rollback", updated2.RecipeState)
	}
	var auditCount int64
	if err := fixture.db.Model(&model.AuditLog{}).
		Where("entity_type = ? AND entity_id IN ?", "culture_recipe", []uint{v1.ID, v2.ID}).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	// v1 validated/published and v2 validated = 3 transition audits; the
	// rejected release must not leave a publish/auto_obsolete audit behind.
	if auditCount != 3 {
		t.Fatalf("audit rows after blocked publish=%d, want 3 (full rollback)", auditCount)
	}
}

func TestPublishBlockedByUnconfirmedAnalysis(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
	transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
	series := createReadySeries(t, fixture, v1)
	analysis := createOpenAnalysis(t, fixture, series, v1, "reviewed")
	t.Cleanup(func() { _ = analysis.ID })
	v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
	transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)

	_, err := fixture.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 2,
	}, fixture.actor)
	var blocked *PublishBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("want PublishBlockedError, got %v", err)
	}
	var kinds []string
	for _, blocker := range blocked.Payload.Blockers {
		kinds = append(kinds, blocker.Kind)
	}
	if len(kinds) != 2 || kinds[0] != "ready_series" || kinds[1] != "open_analysis" {
		t.Fatalf("blocker kinds=%v, want [ready_series open_analysis]", kinds)
	}
}

func TestPublishAllowedAfterReferencesCleared(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
	transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
	series := createReadySeries(t, fixture, v1)
	v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
	transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)

	_, err := fixture.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 2,
	}, fixture.actor)
	if err == nil {
		t.Fatal("expected publish to be blocked while ready series exists")
	}
	if changed, transitionErr := fixture.seriesRepo.Transition(context.Background(), series.ID,
		"ready", "superseded", "", "", time.Now().UTC()); transitionErr != nil || !changed {
		t.Fatalf("supersede series: changed=%v err=%v", changed, transitionErr)
	}
	transitionRecipe(t, fixture, v2.ID, constants.RecipePublished, 2)
	updated1, _ := fixture.recipeRepo.GetByID(context.Background(), v1.ID, false)
	updated2, _ := fixture.recipeRepo.GetByID(context.Background(), v2.ID, false)
	if updated1.RecipeState != "obsolete" || updated2.RecipeState != "published" {
		t.Fatalf("states after cleared gate: v1=%s v2=%s", updated1.RecipeState, updated2.RecipeState)
	}
}

func TestConfirmedAndVoidedAnalysesDoNotBlock(t *testing.T) {
	for _, terminal := range []string{"confirmed", "voided"} {
		t.Run(terminal, func(t *testing.T) {
			fixture := setupRecipeGateFixture(t)
			v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
			transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
			transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
			series := createReadySeries(t, fixture, v1)
			analysis := createOpenAnalysis(t, fixture, series, v1, terminal)
			// Remove the ready-series blocker, leave the terminal analysis.
			if _, err := fixture.seriesRepo.Transition(context.Background(), series.ID,
				"ready", "superseded", "", "", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
			transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)
			transitionRecipe(t, fixture, v2.ID, constants.RecipePublished, 2)
			_ = analysis
		})
	}
}

func TestPublishGateProjectionOnListAndDetail(t *testing.T) {
	fixture := setupRecipeGateFixture(t)
	v1 := createRecipeVersion(t, fixture, 1, constants.RecipeDraft)
	transitionRecipe(t, fixture, v1.ID, constants.RecipeValidated, 1)
	transitionRecipe(t, fixture, v1.ID, constants.RecipePublished, 1)
	series := createReadySeries(t, fixture, v1)
	v2 := createRecipeVersion(t, fixture, 2, constants.RecipeDraft)
	transitionRecipe(t, fixture, v2.ID, constants.RecipeValidated, 2)

	detail, err := fixture.svc.Get(context.Background(), v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.PublishGate == nil || !detail.PublishGate.Blocked || len(detail.PublishGate.Blockers) != 1 {
		t.Fatalf("validated v2 detail gate=%+v, want blocked with 1 blocker", detail.PublishGate)
	}
	list, err := fixture.svc.List(context.Background(), dto.CultureRecipeQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	var blockedCount, liveRefCount int
	for _, item := range list.Items {
		if item.Version == 2 && item.PublishGate != nil && item.PublishGate.Blocked {
			blockedCount++
		}
		if item.Version == 1 && item.PublishGate != nil && len(item.PublishGate.Blockers) == 1 {
			liveRefCount++
		}
	}
	if blockedCount != 1 || liveRefCount != 1 {
		t.Fatalf("list projection blocked=%d liveRef=%d, want 1/1", blockedCount, liveRefCount)
	}
	_ = series
}

// barrierTx runs the unit of work on a real transaction but parks after the
// work completes and before the transaction commits, when the gate epoch has
// already advanced, so two publishes genuinely overlap.
type barrierTx struct {
	db       *gorm.DB
	committed chan struct{}
	release  chan struct{}
}

func newBarrierTx(db *gorm.DB) *barrierTx {
	return &barrierTx{db: db, committed: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *barrierTx) InTx(ctx context.Context, fn func(context.Context) error) error {
	var txErr error
	commitErr := b.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fn(repository.WithTx(ctx, tx)); err != nil {
			txErr = err
			return err
		}
		b.committed <- struct{}{}
		<-b.release
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return commitErr
}

func TestConcurrentPublishOnlyOneSucceeds(t *testing.T) {
	db := newConcurrentTestDB(t)
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	gateRepo := repository.NewRecipePublishGateRepository(db)
	vessel := model.FermentationVessel{
		VesselCode: "FV-C1", Name: "Concurrent vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	actor := util.Actor{UserID: 1, Username: "scientist", Role: string(constants.RoleProcessScientist), RequestID: "req-concurrent"}
	bootstrap := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, repository.NewTxRunner(db))
	boundaries, references, tolerances := testRecipeConfig(t)
	created, err := bootstrap.Create(context.Background(), dto.CreateCultureRecipeRequest{
		VesselID: vessel.ID, RecipeCode: "GATE-C", Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: boundaries, ReferenceCurvesJSON: references, ToleranceProfileJSON: tolerances,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "validated", Version: 1,
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 1,
	}, actor); err != nil {
		t.Fatal(err)
	}
	copied, err := bootstrap.Copy(context.Background(), created.ID, dto.CopyCultureRecipeRequest{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
		ToState: "validated", Version: 2,
	}, actor); err != nil {
		t.Fatal(err)
	}

	winnerBarrier := newBarrierTx(db)
	loserBarrier := newBarrierTx(db)
	winnerSvc := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, winnerBarrier)
	loserSvc := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, loserBarrier)
	errs := make(chan error, 2)
	go func() {
		_, publishErr := winnerSvc.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
			ToState: "published", Version: 2,
		}, actor)
		errs <- publishErr
	}()
	<-winnerBarrier.committed
	go func() {
		_, publishErr := loserSvc.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
			ToState: "published", Version: 2,
		}, actor)
		errs <- publishErr
	}()
	// Loser completes its gate work while the winner transaction is still open.
	select {
	case loserErr := <-errs:
		var appErr *util.AppError
		if !errors.As(loserErr, &appErr) || appErr.Status != http.StatusConflict {
			t.Fatalf("losing publish err=%v, want 409 conflict", loserErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("losing publish did not resolve while winner held the gate")
	}
	close(winnerBarrier.release)
	if winnerErr := <-errs; winnerErr != nil {
		t.Fatalf("winning publish failed: %v", winnerErr)
	}

	publishedCount := int64(0)
	if err := db.Model(&model.CultureRecipe{}).
		Where("vessel_id = ? AND recipe_code = ? AND recipe_state = ?", vessel.ID, "GATE-C", "published").
		Count(&publishedCount).Error; err != nil {
		t.Fatal(err)
	}
	if publishedCount != 1 {
		t.Fatalf("published versions=%d, want exactly 1", publishedCount)
	}
	versions, _, err := recipeRepo.List(context.Background(), dto.CultureRecipeQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if versions[0].RecipeState != "published" || versions[1].RecipeState != "obsolete" {
		t.Fatalf("final states v1=%s v2=%s, want v1 obsolete and v2 published",
			versions[1].RecipeState, versions[0].RecipeState)
	}
}

// TestConcurrentPublishAndObsoleteOnlyOneSucceeds proves a manual obsolete
// racing a release cannot leave the group in a split state.
func TestConcurrentPublishAndObsoleteOnlyOneSucceeds(t *testing.T) {
	db := newConcurrentTestDB(t)
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	gateRepo := repository.NewRecipePublishGateRepository(db)
	vessel := model.FermentationVessel{
		VesselCode: "FV-C2", Name: "Obsolete race vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	actor := util.Actor{UserID: 1, Username: "scientist", Role: string(constants.RoleProcessScientist), RequestID: "req-race-obsolete"}
	bootstrap := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, repository.NewTxRunner(db))
	boundaries, references, tolerances := testRecipeConfig(t)
	created, err := bootstrap.Create(context.Background(), dto.CreateCultureRecipeRequest{
		VesselID: vessel.ID, RecipeCode: "GATE-D", Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: boundaries, ReferenceCurvesJSON: references, ToleranceProfileJSON: tolerances,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "validated", Version: 1,
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 1,
	}, actor); err != nil {
		t.Fatal(err)
	}
	copied, err := bootstrap.Copy(context.Background(), created.ID, dto.CopyCultureRecipeRequest{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
		ToState: "validated", Version: 2,
	}, actor); err != nil {
		t.Fatal(err)
	}

	publishBarrier := newBarrierTx(db)
	publishSvc := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, publishBarrier)
	obsoleteSvc := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, gateRepo, auditRepo, repository.NewTxRunner(db))
	errs := make(chan error, 2)
	go func() {
		_, publishErr := publishSvc.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
			ToState: "published", Version: 2,
		}, actor)
		errs <- publishErr
	}()
	<-publishBarrier.committed
	_, obsoleteErr := obsoleteSvc.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "obsolete", Version: 1,
	}, actor)
	close(publishBarrier.release)
	publishErr := <-errs
	if publishErr == nil && obsoleteErr == nil {
		t.Fatal("concurrent publish and obsolete cannot both succeed")
	}
	publishedCount := int64(0)
	if err := db.Model(&model.CultureRecipe{}).
		Where("vessel_id = ? AND recipe_code = ? AND recipe_state = ?", vessel.ID, "GATE-D", "published").
		Count(&publishedCount).Error; err != nil {
		t.Fatal(err)
	}
	if publishedCount != 1 {
		t.Fatalf("published versions=%d, want exactly 1 after the race", publishedCount)
	}
}

func TestPublishBlockedErrorClassifiesAs409(t *testing.T) {
	err := &PublishBlockedError{
		AppError: util.NewError(http.StatusConflict, util.CodePublishBlocked, "blocked"),
		Payload:  dto.PublishBlockedPayload{BlockedRecipeID: 9},
	}
	var appErr *util.AppError
	if !errors.As(err, &appErr) {
		t.Fatal("PublishBlockedError must unwrap to *util.AppError")
	}
	if appErr.Status != http.StatusConflict || appErr.Code != util.CodePublishBlocked {
		t.Fatalf("status=%d code=%s", appErr.Status, appErr.Code)
	}
	var carrier util.DetailCarrier
	if !errors.As(err, &carrier) {
		t.Fatal("PublishBlockedError must expose details")
	}
	payload, ok := carrier.ErrorDetails().(dto.PublishBlockedPayload)
	if !ok || payload.BlockedRecipeID != 9 {
		t.Fatalf("details=%v", carrier.ErrorDetails())
	}
}

func createReadySeries(t *testing.T, fixture recipeGateFixture, recipe model.CultureRecipe) model.SensorSeries {
	t.Helper()
	now := time.Now().UTC()
	series := model.SensorSeries{
		VesselID: fixture.vessel.ID, RecipeID: recipe.ID, RunCode: "RUN-READY-1", Channel: "multichannel",
		SampleIntervalS: 3600, PointsJSON: `[]`, StartedAt: now.Add(-time.Hour), EndedAt: now,
		SourceChecksum: "checksum-ready", SeriesState: "ready",
		QualitySummary: `{"valid":true,"warnings":[],"channels":["ph"]}`, NormalizationJSON: "{}",
		ImportedBy: fixture.actor.UserID, ImportedByName: fixture.actor.Username, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&series).Error; err != nil {
		t.Fatalf("create ready series: %v", err)
	}
	return series
}

func createOpenAnalysis(t *testing.T, fixture recipeGateFixture, series model.SensorSeries, recipe model.CultureRecipe, state string) model.DeviationAnalysis {
	t.Helper()
	now := time.Now().UTC()
	analysis := model.DeviationAnalysis{
		SensorSeriesID: series.ID, RecipeID: recipe.ID, RecipeVersion: recipe.Version,
		AlgorithmVersion: algorithm.Version, InputHash: "hash-" + state + "-" + recipe.RecipeCode,
		InputSnapshot: "{}", PhaseScoresJSON: "[]", DeviationLevel: "normal",
		AlignedCurveJSON: "[]", SuspectedCausesJSON: "[]", AnalysisState: state,
		Explanation: "fixture", AnalyzedAt: now, InitiatedBy: fixture.actor.UserID,
		InitiatedByName: fixture.actor.Username, IdempotencyKey: "idem-" + state + "-" + recipe.RecipeCode,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&analysis).Error; err != nil {
		t.Fatalf("create %s analysis: %v", state, err)
	}
	return analysis
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// newConcurrentTestDB opens an on-disk WAL SQLite database that accepts
// overlapping writer transactions, required to exercise the publish gate
// under real concurrency.
func newConcurrentTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.TempDir()+"/gate.db?_journal_mode=WAL&_busy_timeout=1000"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareSchema(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func prepareSchema(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.User{}, &model.FermentationVessel{}, &model.CultureRecipe{},
		&model.SensorSeries{}, &model.DeviationAnalysis{}, &model.AuditLog{},
		&model.RecipePublishGate{},
	); err != nil {
		return err
	}
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_culture_recipes_one_published " +
		"ON culture_recipes (vessel_id, recipe_code) WHERE recipe_state = 'published'").Error
}

func testRecipeConfig(t *testing.T) (json.RawMessage, json.RawMessage, json.RawMessage) {
	t.Helper()
	boundaries, err := json.Marshal([]algorithm.PhaseBoundary{
		{Phase: constants.PhaseLag, StartHour: 0, EndHour: 2},
		{Phase: constants.PhaseGrowth, StartHour: 2, EndHour: 4},
		{Phase: constants.PhaseProduction, StartHour: 4, EndHour: 6},
		{Phase: constants.PhaseHarvest, StartHour: 6, EndHour: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	curves := map[string][]algorithm.CurvePoint{"ph": {}}
	for hour := 0; hour <= 8; hour++ {
		curves["ph"] = append(curves["ph"], algorithm.CurvePoint{ElapsedHour: float64(hour), Value: 7 - float64(hour)*0.05})
	}
	references, err := json.Marshal(curves)
	if err != nil {
		t.Fatal(err)
	}
	tolerances, err := json.Marshal(map[string]algorithm.ChannelTolerance{"ph": {Weight: 1, MaxDistance: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return boundaries, references, tolerances
}
