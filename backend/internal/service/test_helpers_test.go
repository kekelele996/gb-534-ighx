package service

import (
	"encoding/json"
	"testing"

	"fermentation-kinetics-deviation-analysis/backend/internal/algorithm"
	"fermentation-kinetics-deviation-analysis/backend/internal/constants"
	"fermentation-kinetics-deviation-analysis/backend/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.FermentationVessel{}, &model.CultureRecipe{},
		&model.SensorSeries{}, &model.DeviationAnalysis{}, &model.AuditLog{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_recipe_single_published " +
		"ON culture_recipes (vessel_id, recipe_code) WHERE recipe_state = 'published'").Error; err != nil {
		t.Fatal(err)
	}
	return db
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
