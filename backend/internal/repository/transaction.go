package repository

import (
	"context"

	"gorm.io/gorm"
)

// TxRepositories bundles repository handles bound to the same database
// transaction, so a recipe publish gate can update several rows and write the
// audit trail atomically.
type TxRepositories struct {
	Recipes  CultureRecipeRepository
	Series   SensorSeriesRepository
	Analyses DeviationAnalysisRepository
	Audits   AuditRepository
}

// TransactionManager runs a function inside a single database transaction and
// hands it repository implementations that participate in that transaction.
type TransactionManager interface {
	WithinTransaction(context.Context, func(TxRepositories) error) error
}

// TransactionManagerOption customizes the repositories built inside a
// transaction. It is used in tests to inject failing collaborators; production
// wiring relies entirely on the defaults.
type TransactionManagerOption func(*gormTransactionManager)

// WithTransactionAuditFactory overrides how the in-transaction audit
// repository is constructed.
func WithTransactionAuditFactory(factory func(*gorm.DB) AuditRepository) TransactionManagerOption {
	return func(m *gormTransactionManager) {
		if factory != nil {
			m.auditFactory = factory
		}
	}
}

type gormTransactionManager struct {
	db           *gorm.DB
	auditFactory func(*gorm.DB) AuditRepository
}

// NewTransactionManager builds the GORM-backed transaction manager.
func NewTransactionManager(db *gorm.DB, options ...TransactionManagerOption) TransactionManager {
	manager := &gormTransactionManager{db: db, auditFactory: NewAuditRepository}
	for _, option := range options {
		option(manager)
	}
	return manager
}

func (m *gormTransactionManager) WithinTransaction(
	ctx context.Context, fn func(TxRepositories) error,
) error {
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		bundle := TxRepositories{
			Recipes:  NewCultureRecipeRepository(tx),
			Series:   NewSensorSeriesRepository(tx),
			Analyses: NewDeviationAnalysisRepository(tx),
			Audits:   m.auditFactory(tx),
		}
		return fn(bundle)
	})
}
