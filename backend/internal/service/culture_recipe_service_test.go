package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/dto"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"fermentation-kinetics-deviation-analysis/backend/internal/repository"
	"fermentation-kinetics-deviation-analysis/backend/internal/util"
	"gorm.io/gorm"
)

func TestCultureRecipeLifecycleAndVersionCopy(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	created, err := h.svc.Create(context.Background(), dto.CreateCultureRecipeRequest{
		VesselID: h.vessel.ID, RecipeCode: "TEST-A", Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: h.boundaries, ReferenceCurvesJSON: h.references, ToleranceProfileJSON: h.tolerances,
	}, h.actor)
	if err != nil {
		t.Fatalf("create recipe: %v", err)
	}
	validated, err := h.svc.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "validated", Version: 1,
	}, h.actor)
	if err != nil || validated.Recipe.RecipeState != "validated" {
		t.Fatalf("validate recipe: state=%s err=%v", validated.Recipe.RecipeState, err)
	}
	published, err := h.svc.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 1,
	}, h.actor)
	if err != nil || published.Recipe.RecipeState != "published" {
		t.Fatalf("publish recipe: state=%s err=%v", published.Recipe.RecipeState, err)
	}
	copied, err := h.svc.Copy(context.Background(), created.ID, dto.CopyCultureRecipeRequest{}, h.actor)
	if err != nil {
		t.Fatalf("copy recipe: %v", err)
	}
	if copied.Version != 2 || copied.RecipeState != "draft" {
		t.Fatalf("copy version=%d state=%s, want 2 draft", copied.Version, copied.RecipeState)
	}
}

type recipeTestHarness struct {
	db                                 *gorm.DB
	svc                                *CultureRecipeService
	vessel                             model.FermentationVessel
	boundaries, references, tolerances json.RawMessage
	actor                              util.Actor
}

func newRecipeTestHarness(t *testing.T, db *gorm.DB) recipeTestHarness {
	t.Helper()
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	txManager := repository.NewTransactionManager(db)
	vessel := model.FermentationVessel{
		VesselCode: "FV-T1", Name: "Test vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	boundaries, references, tolerances := testRecipeConfig(t)
	svc := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, auditRepo, txManager)
	actor := util.Actor{UserID: 1, Username: "scientist", Role: string(constants.RoleProcessScientist), RequestID: "req-test"}
	return recipeTestHarness{
		db: db, svc: svc, vessel: vessel,
		boundaries: boundaries, references: references, tolerances: tolerances, actor: actor,
	}
}

// publishV1ThenDraftV2 creates a published v1 and a copied validated v2 ready
// to be published.
func (h recipeTestHarness) publishV1ThenDraftV2(t *testing.T, code string) (v1, v2 model.CultureRecipe) {
	t.Helper()
	ctx := context.Background()
	created, err := h.svc.Create(ctx, dto.CreateCultureRecipeRequest{
		VesselID: h.vessel.ID, RecipeCode: code, Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: h.boundaries, ReferenceCurvesJSON: h.references, ToleranceProfileJSON: h.tolerances,
	}, h.actor)
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := h.svc.Transition(ctx, created.ID, dto.CultureRecipeTransitionRequest{ToState: "validated", Version: 1}, h.actor); err != nil {
		t.Fatalf("validate v1: %v", err)
	}
	if _, err := h.svc.Transition(ctx, created.ID, dto.CultureRecipeTransitionRequest{ToState: "published", Version: 1}, h.actor); err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	copied, err := h.svc.Copy(ctx, created.ID, dto.CopyCultureRecipeRequest{}, h.actor)
	if err != nil {
		t.Fatalf("copy v2: %v", err)
	}
	if _, err := h.svc.Transition(ctx, copied.ID, dto.CultureRecipeTransitionRequest{ToState: "validated", Version: copied.Version}, h.actor); err != nil {
		t.Fatalf("validate v2: %v", err)
	}
	v1, _ = repository.NewCultureRecipeRepository(h.db).GetByID(ctx, created.ID, false)
	v2, _ = repository.NewCultureRecipeRepository(h.db).GetByID(ctx, copied.ID, false)
	return v1, v2
}

func insertReadySeries(t *testing.T, db *gorm.DB, recipeID uint, runCode string) model.SensorSeries {
	t.Helper()
	now := time.Now().UTC()
	series := model.SensorSeries{
		VesselID: 1, RecipeID: recipeID, RunCode: runCode, Channel: "multichannel", SampleIntervalS: 7200,
		PointsJSON: "[]", StartedAt: now.Add(-time.Hour), EndedAt: now, SourceChecksum: "checksum-" + runCode,
		SeriesState: string(constants.SeriesReady), QualitySummary: "{}", NormalizationJSON: "{}",
		ImportedBy: 1, ImportedByName: "analyst", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&series).Error; err != nil {
		t.Fatalf("insert ready series: %v", err)
	}
	return series
}

func insertAnalysis(t *testing.T, db *gorm.DB, recipeID uint, recipeVersion int, state, key string) model.DeviationAnalysis {
	t.Helper()
	now := time.Now().UTC()
	analysis := model.DeviationAnalysis{
		SensorSeriesID: 1, RecipeID: recipeID, RecipeVersion: recipeVersion, AlgorithmVersion: algorithm.Version,
		InputHash: util.HashString(key), InputSnapshot: "{}", PhaseScoresJSON: "[]",
		DeviationLevel: string(constants.DeviationNormal), AlignedCurveJSON: "[]", SuspectedCausesJSON: "[]",
		AnalysisState: state, Explanation: "test", AnalyzedAt: now, InitiatedBy: 2, InitiatedByName: "analyst",
		IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&analysis).Error; err != nil {
		t.Fatalf("insert analysis: %v", err)
	}
	return analysis
}

// With no in-use references, publishing v2 publishes it and obsoletes v1 in one
// atomic switch.
func TestPublishAutoObsoletesPreviousVersion(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-OK")
	result, err := h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: v2.Version,
	}, h.actor)
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if result.Recipe.RecipeState != "published" {
		t.Fatalf("v2 state=%s want published", result.Recipe.RecipeState)
	}
	if len(result.ObsoletedVersions) != 1 || result.ObsoletedVersions[0].ID != v1.ID {
		t.Fatalf("obsoleted=%+v want only v1", result.ObsoletedVersions)
	}
	repo := repository.NewCultureRecipeRepository(db)
	reloadedV1, err := repo.GetByID(context.Background(), v1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	reloadedV2, err := repo.GetByID(context.Background(), v2.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedV1.RecipeState != "obsolete" {
		t.Fatalf("v1 state=%s want obsolete", reloadedV1.RecipeState)
	}
	if reloadedV2.RecipeState != "published" {
		t.Fatalf("v2 state=%s want published after refresh", reloadedV2.RecipeState)
	}
}

// A ready sensor series referencing the old published version blocks the whole
// publish, and neither version changes state.
func TestPublishBlockedByReadySeries(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-SERIES")
	insertReadySeries(t, db, v1.ID, "RUN-READY-1")
	_, err := h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: v2.Version,
	}, h.actor)
	assertGateBlocked(t, err, "ready_series", 1, 0)
	assertStatesUnchanged(t, db, v1.ID, "published", v2.ID, "validated")
}

// An unconfirmed analysis blocks; confirmed/failed/voided analyses do not.
func TestPublishBlockedByUnconfirmedAnalysis(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-ANALYSIS")
	insertAnalysis(t, db, v1.ID, v1.Version, string(constants.AnalysisCompleted), "open-key")
	_, err := h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: v2.Version,
	}, h.actor)
	assertGateBlocked(t, err, "open_analysis", 0, 1)
	assertStatesUnchanged(t, db, v1.ID, "published", v2.ID, "validated")

	// Move the analysis to confirmed; the publish must now succeed.
	if err := db.Model(&model.DeviationAnalysis{}).Where("idempotency_key = ?", "open-key").
		Update("analysis_state", string(constants.AnalysisConfirmed)).Error; err != nil {
		t.Fatal(err)
	}
	result, err := h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: v2.Version,
	}, h.actor)
	if err != nil {
		t.Fatalf("publish after confirmation: %v", err)
	}
	if result.Recipe.RecipeState != "published" {
		t.Fatalf("v2 state=%s want published", result.Recipe.RecipeState)
	}
}

// Terminal analyses never block the switch.
func TestPublishAllowedWithTerminalAnalyses(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-TERMINAL")
	insertAnalysis(t, db, v1.ID, v1.Version, string(constants.AnalysisVoided), "voided-key")
	insertAnalysis(t, db, v1.ID, v1.Version, string(constants.AnalysisFailed), "failed-key")
	result, err := h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: v2.Version,
	}, h.actor)
	if err != nil {
		t.Fatalf("publish with terminal analyses: %v", err)
	}
	if result.Recipe.RecipeState != "published" {
		t.Fatalf("v2 state=%s want published", result.Recipe.RecipeState)
	}
}

func assertGateBlocked(t *testing.T, err error, kind string, readyCount, openCount int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected publish to be blocked, got success")
	}
	appErr, ok := err.(*util.AppError)
	if !ok {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	if appErr.Code != util.CodePublishBlocked {
		t.Fatalf("error code=%s want %s", appErr.Code, util.CodePublishBlocked)
	}
	gate, ok := appErr.Details.(dto.RecipePublishGate)
	if !ok {
		t.Fatalf("expected RecipePublishGate details, got %T", appErr.Details)
	}
	if gate.ReadySeriesCount != readyCount || gate.OpenAnalysisCount != openCount {
		t.Fatalf("counts ready=%d open=%d, want %d/%d", gate.ReadySeriesCount, gate.OpenAnalysisCount, readyCount, openCount)
	}
	found := false
	for _, blocker := range gate.Blockers {
		if blocker.Kind == kind {
			found = true
			if blocker.Reason == "" || blocker.Label == "" {
				t.Fatalf("blocker missing reason/label: %+v", blocker)
			}
		}
	}
	if !found {
		t.Fatalf("blocker kind %s not found in %+v", kind, gate.Blockers)
	}
}

func assertStatesUnchanged(t *testing.T, db *gorm.DB, v1ID uint, v1State string, v2ID uint, v2State string) {
	t.Helper()
	repo := repository.NewCultureRecipeRepository(db)
	v1, err := repo.GetByID(context.Background(), v1ID, false)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := repo.GetByID(context.Background(), v2ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if v1.RecipeState != v1State {
		t.Fatalf("v1 state=%s want %s (switch must roll back)", v1.RecipeState, v1State)
	}
	if v2.RecipeState != v2State {
		t.Fatalf("v2 state=%s want %s (blocked publish must not partially apply)", v2.RecipeState, v2State)
	}
}

// Two concurrent publish requests for the same new version must result in
// exactly one success and exactly one published version in the group.
func TestConcurrentPublishSucceedsOnce(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-CONC")
	key := fmt.Sprintf("%d/%s", h.vessel.ID, "GATE-CONC")
	holdUntilBothQueued(t, h.svc, key)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, results[index] = h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
				ToState: "published", Version: v2.Version,
			}, h.actor)
		}(i)
	}
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent publish successes=%d want exactly 1 (errors=%v)", successes, results)
	}
	repo := repository.NewCultureRecipeRepository(db)
	published, err := repo.ListPublishedInGroup(context.Background(), h.vessel.ID, "GATE-CONC", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].ID != v2.ID {
		t.Fatalf("published versions after concurrent publish=%+v want only v2", published)
	}
	v1Reload, _ := repo.GetByID(context.Background(), v1.ID, false)
	if v1Reload.RecipeState != "obsolete" {
		t.Fatalf("v1 state=%s want obsolete", v1Reload.RecipeState)
	}
}

// Concurrent publish of a new version and manual obsoletion of the current
// version must produce exactly one winning state change.
func TestConcurrentPublishAndObsoleteSucceedsOnce(t *testing.T) {
	db := newTestDB(t)
	h := newRecipeTestHarness(t, db)
	v1, v2 := h.publishV1ThenDraftV2(t, "GATE-RACE")
	key := fmt.Sprintf("%d/%s", h.vessel.ID, "GATE-RACE")
	holdUntilBothQueued(t, h.svc, key)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = h.svc.Transition(context.Background(), v2.ID, dto.CultureRecipeTransitionRequest{
			ToState: "published", Version: v2.Version,
		}, h.actor)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = h.svc.Transition(context.Background(), v1.ID, dto.CultureRecipeTransitionRequest{
			ToState: "obsolete", Version: v1.Version,
		}, h.actor)
	}()
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent publish/obsolete successes=%d want exactly 1 (errors=%v)", successes, errs)
	}
	repo := repository.NewCultureRecipeRepository(db)
	v1Reload, _ := repo.GetByID(context.Background(), v1.ID, false)
	v2Reload, _ := repo.GetByID(context.Background(), v2.ID, false)
	publishedCount := 0
	if v1Reload.RecipeState == "published" {
		publishedCount++
	}
	if v2Reload.RecipeState == "published" {
		publishedCount++
	}
	if publishedCount > 1 {
		t.Fatalf("two published versions after race: v1=%s v2=%s", v1Reload.RecipeState, v2Reload.RecipeState)
	}
}

// holdUntilBothQueued installs the service test hook so that, for one switch
// request, the first goroutine to hold the group slot waits until a second
// request has registered. This makes the two requests genuinely overlap at the
// gate instead of racing the Go scheduler.
func holdUntilBothQueued(t *testing.T, svc *CultureRecipeService, key string) {
	t.Helper()
	var once sync.Once
	svc.onSwitchHeld = func(heldKey string) {
		if heldKey != key {
			return
		}
		once.Do(func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && svc.switchGate.Waiting(key) < 2 {
				time.Sleep(time.Millisecond)
			}
		})
	}
	t.Cleanup(func() { svc.onSwitchHeld = nil })
}

// failingAuditRepo wraps AuditRepository and fails on Record, used to prove a
// failure after the two recipe state updates rolls the whole publish switch
// back (old version stays published, new version stays validated).
type failingAuditRepo struct {
	repository.AuditRepository
}

func (f failingAuditRepo) Record(_ context.Context, _ model.AuditLog) error {
	return errors.New("forced audit failure")
}

func TestPublishRollsBackWhenAuditFails(t *testing.T) {
	db := newTestDB(t)
	vesselRepo := repository.NewFermentationVesselRepository(db)
	recipeRepo := repository.NewCultureRecipeRepository(db)
	seriesRepo := repository.NewSensorSeriesRepository(db)
	analysisRepo := repository.NewDeviationAnalysisRepository(db)
	auditRepo := repository.NewAuditRepository(db)
	vessel := model.FermentationVessel{
		VesselCode: "FV-TX", Name: "Rollback vessel", WorkingVolumeL: 100,
		SensorChannels: `["ph"]`, Location: "Lab", OwnerTeam: "Process",
		VesselState: "active", CommissionedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := vesselRepo.Create(context.Background(), &vessel); err != nil {
		t.Fatal(err)
	}
	boundaries, references, tolerances := testRecipeConfig(t)
	// Service used for setup writes audits normally.
	setup := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, auditRepo,
		repository.NewTransactionManager(db))
	actor := util.Actor{UserID: 1, Username: "scientist", Role: string(constants.RoleProcessScientist), RequestID: "req-rb"}
	created, err := setup.Create(context.Background(), dto.CreateCultureRecipeRequest{
		VesselID: vessel.ID, RecipeCode: "GATE-ROLLBACK", Organism: "Test organism", TargetDurationH: 8,
		PhaseBoundariesJSON: boundaries, ReferenceCurvesJSON: references, ToleranceProfileJSON: tolerances,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{ToState: "validated", Version: 1}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Transition(context.Background(), created.ID, dto.CultureRecipeTransitionRequest{ToState: "published", Version: 1}, actor); err != nil {
		t.Fatal(err)
	}
	copied, err := setup.Copy(context.Background(), created.ID, dto.CopyCultureRecipeRequest{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{ToState: "validated", Version: 2}, actor); err != nil {
		t.Fatal(err)
	}
	// Failing service reuses the same database but makes every audit Record
	// fail inside the publish transaction via a transaction-manager override.
	failingManager := repository.NewTransactionManager(db, repository.WithTransactionAuditFactory(
		func(_ *gorm.DB) repository.AuditRepository {
			return failingAuditRepo{AuditRepository: auditRepo}
		},
	))
	failing := NewCultureRecipeService(recipeRepo, vesselRepo, seriesRepo, analysisRepo, auditRepo, failingManager)
	_, err = failing.Transition(context.Background(), copied.ID, dto.CultureRecipeTransitionRequest{
		ToState: "published", Version: 2,
	}, actor)
	if err == nil {
		t.Fatal("expected publish to fail when audit recording fails")
	}
	repo := repository.NewCultureRecipeRepository(db)
	oldVersion, err := repo.GetByID(context.Background(), created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := repo.GetByID(context.Background(), copied.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if oldVersion.RecipeState != "published" {
		t.Fatalf("old version state=%s want published after rollback", oldVersion.RecipeState)
	}
	if newVersion.RecipeState != "validated" {
		t.Fatalf("new version state=%s want validated after rollback", newVersion.RecipeState)
	}
}
