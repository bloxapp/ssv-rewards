package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/bloxapp/ssv-rewards/pkg/models"
	"github.com/bloxapp/ssv-rewards/pkg/rewards"
	"github.com/gocarina/gocsv"
	"github.com/volatiletech/sqlboiler/v4/queries"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
)

type CalcCmd struct {
	Dir                      string `default:"./rewards" help:"Path to save the rewards to,"`
	PerformanceProvider      string `default:"beaconcha" help:"Performance provider to use."                                       enum:"beaconcha,e2m"`
	MinimumDailyAttestations int    `default:"202"       help:"Minimum attestations in a day to be considered active."`

	db *sql.DB
}

func (c *CalcCmd) Run(logger *zap.Logger, globals *Globals) error {
	ctx := context.Background()

	// Parse the rewards plan.
	data, err := os.ReadFile("rewards.yaml")
	if err != nil {
		return fmt.Errorf("failed to read rewards.yaml: %w", err)
	}
	plan, err := rewards.ParseYAML(data)
	if err != nil {
		return fmt.Errorf("failed to parse rewards plan: %w", err)
	}

	// Connect to the PostgreSQL database.
	c.db, err = sql.Open("postgres", globals.Postgres)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	logger.Info("Connected to PostgreSQL")

	// Verify that validator performance data is available.
	state, err := models.States().One(ctx, c.db)
	if err != nil {
		return fmt.Errorf("failed to get state: %w", err)
	}
	if state.EarliestValidatorPerformance.IsZero() || state.LatestValidatorPerformance.IsZero() {
		return fmt.Errorf("validator performance data is not available")
	}
	if state.EarliestValidatorPerformance.Time.After(state.LatestValidatorPerformance.Time) {
		return fmt.Errorf("invalid state: earliest validator performance is after latest validator performance")
	}
	if state.EarliestValidatorPerformance.Time.After(plan.Rounds[0].Period.FirstDay()) {
		return fmt.Errorf("validator performance data is not available for the first round")
	}
	latestValidatorPerformancePeriod := rewards.PeriodAt(state.LatestValidatorPerformance.Time)

	// Select the rounds with available performance data.
	var completeRounds []rewards.Round
	for _, round := range plan.Rounds {
		if round.ETHAPR > 0 && round.SSVETH > 0 &&
			round.Period.LastDay().Before(latestValidatorPerformancePeriod.FirstDay()) {
			completeRounds = append(completeRounds, round)
		}
	}

	// Calculate rewards.
	var byValidator []*ValidatorParticipationRound
	var byOwner []*OwnerParticipationRound
	var totalByValidator = map[string]*ValidatorParticipation{}
	var totalByOwner = map[string]*OwnerParticipation{}
	for _, round := range completeRounds {
		// Collect validator and owner participations.
		validatorParticipations, err := c.validatorParticipations(ctx, round.Period)
		if err != nil {
			return fmt.Errorf("failed to get validator participations: %w", err)
		}
		ownerParticipations, err := c.ownerParticipations(ctx, round.Period)
		if err != nil {
			return fmt.Errorf("failed to get owner participations: %w", err)
		}

		// Calculate appropriate tier and rewards.
		tier, err := plan.Tier(len(validatorParticipations))
		if err != nil {
			return fmt.Errorf("failed to get tier: %w", err)
		}
		dailyReward, monthlyReward, annualReward, err := plan.ValidatorRewards(round.Period, len(validatorParticipations))
		if err != nil {
			return fmt.Errorf("failed to get reward: %w", err)
		}

		// Attach rewards to participations.
		ownerRewards := map[string]float64{}
		ownerActiveDays := map[string]int{}
		for _, participation := range validatorParticipations {
			participation.Reward = dailyReward * float64(participation.ActiveDays)
			ownerRewards[participation.OwnerAddress] += participation.Reward
			ownerActiveDays[participation.OwnerAddress] += participation.ActiveDays

			byValidator = append(byValidator, &ValidatorParticipationRound{
				Period:                 round.Period,
				ValidatorParticipation: participation,
			})
			if total, ok := totalByValidator[participation.PublicKey]; ok {
				total.ActiveDays += participation.ActiveDays
				total.Reward += participation.Reward
			} else {
				cpy := *participation
				totalByValidator[participation.PublicKey] = &cpy
			}
		}
		for _, participation := range ownerParticipations {
			participation.Reward = dailyReward * float64(participation.ActiveDays)

			if participation.ActiveDays != ownerActiveDays[participation.OwnerAddress] {
				return fmt.Errorf("inconsistent active days for owner %q", participation.OwnerAddress)
			}

			byOwner = append(byOwner, &OwnerParticipationRound{
				Period:             round.Period,
				OwnerParticipation: participation,
			})
			if total, ok := totalByOwner[participation.OwnerAddress]; ok {
				total.ActiveDays += participation.ActiveDays
				total.Reward += participation.Reward
			} else {
				cpy := *participation
				totalByOwner[participation.OwnerAddress] = &cpy
			}
		}

		// Export rewards.
		dir := filepath.Join(c.Dir, round.Period.String())
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
		if err := exportCSV(validatorParticipations, filepath.Join(dir, "by-validator.csv")); err != nil {
			return fmt.Errorf("failed to export validator rewards: %w", err)
		}
		if err := exportCSV(ownerParticipations, filepath.Join(dir, "by-owner.csv")); err != nil {
			return fmt.Errorf("failed to export owner rewards: %w", err)
		}

		// Export cumulative rewards.
		totalRewards := map[string]*big.Int{}
		for _, participation := range totalByOwner {
			totalRewards[participation.OwnerAddress], _ = new(big.Float).Mul(
				big.NewFloat(participation.Reward),
				big.NewFloat(math.Pow10(18)),
			).Int(nil)
		}
		f, err := os.Create(filepath.Join(dir, "cumulative.json"))
		if err != nil {
			return fmt.Errorf("failed to create cumulative.json: %w", err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(totalRewards); err != nil {
			return fmt.Errorf("failed to encode total rewards: %w", err)
		}

		logger.Info(
			"Exported rewards for round",
			zap.String("period", round.Period.String()),
			zap.Int("participations", len(validatorParticipations)),
			zap.Int("tier", tier.MaxParticipants),
			zap.Float64("daily_reward", dailyReward),
			zap.Float64("monthly_reward", monthlyReward),
			zap.Float64("annual_reward", annualReward),
		)
	}

	// Export total rewards.
	if err := exportCSV(byValidator, filepath.Join(c.Dir, "by-validator.csv")); err != nil {
		return fmt.Errorf("failed to export total validator rewards: %w", err)
	}
	if err := exportCSV(byOwner, filepath.Join(c.Dir, "by-owner.csv")); err != nil {
		return fmt.Errorf("failed to export total owner rewards: %w", err)
	}
	if err := exportCSV(maps.Values(totalByValidator), filepath.Join(c.Dir, "total-by-validator.csv")); err != nil {
		return fmt.Errorf("failed to export total validator rewards: %w", err)
	}
	if err := exportCSV(maps.Values(totalByOwner), filepath.Join(c.Dir, "total-by-owner.csv")); err != nil {
		return fmt.Errorf("failed to export total owner rewards: %w", err)
	}

	return nil
}

type ValidatorParticipation struct {
	OwnerAddress string
	PublicKey    string
	ActiveDays   int
	Reward       float64 `boil:"-"`
}

type ValidatorParticipationRound struct {
	Period rewards.Period
	*ValidatorParticipation
}

func (c *CalcCmd) validatorParticipations(
	ctx context.Context,
	period rewards.Period,
) ([]*ValidatorParticipation, error) {
	var rewards []*ValidatorParticipation
	return rewards, queries.Raw(
		"SELECT * FROM active_days_by_validator($1, $2, $3)",
		c.PerformanceProvider, c.MinimumDailyAttestations, time.Time(period),
	).Bind(ctx, c.db, &rewards)
}

type OwnerParticipation struct {
	OwnerAddress string
	Validators   int
	ActiveDays   int
	Reward       float64 `boil:"-"`
}

type OwnerParticipationRound struct {
	Period rewards.Period
	*OwnerParticipation
}

func (c *CalcCmd) ownerParticipations(
	ctx context.Context,
	period rewards.Period,
) ([]*OwnerParticipation, error) {
	var rewards []*OwnerParticipation
	return rewards, queries.Raw(
		"SELECT * FROM active_days_by_owner($1, $2, $3)",
		c.PerformanceProvider, c.MinimumDailyAttestations, time.Time(period),
	).Bind(ctx, c.db, &rewards)
}

func exportCSV(data any, fileName string) error {
	f, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("failed to create %q: %w", fileName, err)
	}
	defer f.Close()
	if err := gocsv.Marshal(data, f); err != nil {
		return fmt.Errorf("failed to marshal %q: %w", fileName, err)
	}
	return nil
}
