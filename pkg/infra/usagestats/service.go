package usagestats

import (
	"context"
	"fmt"
	"time"

	"github.com/credativ/plutono/pkg/bus"
	"github.com/credativ/plutono/pkg/login/social"
	"github.com/credativ/plutono/pkg/models"
	"github.com/credativ/plutono/pkg/services/alerting"
	"github.com/credativ/plutono/pkg/services/sqlstore"

	"github.com/credativ/plutono/pkg/infra/log"
	"github.com/credativ/plutono/pkg/registry"
	"github.com/credativ/plutono/pkg/setting"
)

var metricsLogger log.Logger = log.New("metrics")

func init() {
	registry.RegisterService(&UsageStatsService{})
}

type UsageStats interface {
	GetUsageReport(ctx context.Context) (UsageReport, error)

	RegisterMetric(name string, fn MetricFunc)
}

type MetricFunc func() (interface{}, error)

type UsageStatsService struct {
	Cfg                *setting.Cfg               `inject:""`
	Bus                bus.Bus                    `inject:""`
	SQLStore           *sqlstore.SQLStore         `inject:""`
	AlertingUsageStats alerting.UsageStatsQuerier `inject:""`
	License            models.Licensing           `inject:""`

	log log.Logger

	oauthProviders           map[string]bool
	externalMetrics          map[string]MetricFunc
	concurrentUserStatsCache memoConcurrentUserStats
}

func (uss *UsageStatsService) Init() error {
	uss.log = log.New("infra.usagestats")
	uss.oauthProviders = social.GetOAuthProviders(uss.Cfg)
	uss.externalMetrics = make(map[string]MetricFunc)
	return nil
}

func (uss *UsageStatsService) Run(ctx context.Context) error {
	uss.updateTotalStats()

	updateStatsTicker := time.NewTicker(time.Minute * 30)
	defer updateStatsTicker.Stop()

	for {
		select {
		case <-updateStatsTicker.C:
			uss.updateTotalStats()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type memoConcurrentUserStats struct {
	stats *concurrentUsersStats

	memoized time.Time
}

const concurrentUserStatsCacheLifetime = time.Hour

func (uss *UsageStatsService) GetConcurrentUsersStats(ctx context.Context) (*concurrentUsersStats, error) {
	memoizationPeriod := time.Now().Add(-concurrentUserStatsCacheLifetime)
	if !uss.concurrentUserStatsCache.memoized.Before(memoizationPeriod) {
		return uss.concurrentUserStatsCache.stats, nil
	}

	uss.concurrentUserStatsCache.stats = &concurrentUsersStats{}
	err := uss.SQLStore.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		// Retrieves concurrent users stats as a histogram. Buckets are accumulative and upper bound is inclusive.
		rawSQL := `
SELECT
    COUNT(CASE WHEN tokens <= 3 THEN 1 END) AS bucket_le_3,
    COUNT(CASE WHEN tokens <= 6 THEN 1 END) AS bucket_le_6,
    COUNT(CASE WHEN tokens <= 9 THEN 1 END) AS bucket_le_9,
    COUNT(CASE WHEN tokens <= 12 THEN 1 END) AS bucket_le_12,
    COUNT(CASE WHEN tokens <= 15 THEN 1 END) AS bucket_le_15,
    COUNT(1) AS bucket_le_inf
FROM (select count(1) as tokens from user_auth_token group by user_id) uat;`
		_, err := sess.SQL(rawSQL).Get(uss.concurrentUserStatsCache.stats)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get concurrent users stats from database: %w", err)
	}

	uss.concurrentUserStatsCache.memoized = time.Now()
	return uss.concurrentUserStatsCache.stats, nil
}
