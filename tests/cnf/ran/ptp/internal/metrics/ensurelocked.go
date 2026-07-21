package metrics

import (
	"context"
	"fmt"
	"slices"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// EnsureClocksAreLocked ensures that all PTP clocks are locked across all nodes covered by the Prometheus API client.
// It is designed to be used as a BeforeEach/AfterEach check to ensure the cluster is in a stable state.
//
// It ensures that clocks are locked for 10 seconds with a timeout of 5 minutes. It does not check the clock state of
// the chronyd process, as it will be FREERUN when PTP is working correctly.
func EnsureClocksAreLocked(prometheusAPI prometheusv1.API) error {
	query := ClockStateQuery{
		Process: DoesNotEqual(ProcessChronyd),
	}

	err := AssertQuery(context.TODO(), prometheusAPI, query, ClockStateLocked,
		AssertWithStableDuration(10*time.Second),
		AssertWithTimeout(5*time.Minute))
	if err != nil {
		return fmt.Errorf("failed to ensure clocks are locked: %w", err)
	}

	return nil
}

// EnsureClocksAreStable ensures that all PTP clocks are locked across all nodes for a specific continuous duration.
// This is useful for waiting for plugins (e.g. DPLL) to build a sufficient history buffer.
func EnsureClocksAreStable(prometheusAPI prometheusv1.API, stableDuration time.Duration) error {
	query := ClockStateQuery{
		Process: DoesNotEqual(ProcessChronyd),
	}

	err := AssertQuery(context.TODO(), prometheusAPI, query, ClockStateLocked,
		AssertWithStableDuration(stableDuration),
		AssertWithTimeout(stableDuration+5*time.Minute))
	if err != nil {
		return fmt.Errorf("failed to ensure clocks are stable for %s: %w", stableDuration, err)
	}

	return nil
}

// AssertClockClassStable asserts that the clock class metric matching query remained stable for the entire
// window [start, start+duration].
func AssertClockClassStable(
	ctx context.Context,
	client prometheusv1.API,
	query ClockClassQuery,
	expected PtpClockClass,
	start time.Time,
	duration time.Duration,
	step time.Duration,
) error {
	if client == nil {
		return fmt.Errorf("cannot assert clock class stability with nil client")
	}

	rangeQuery := query.ToMetricQuery()
	rangeQuery.Start = start
	rangeQuery.End = start.Add(duration)
	rangeQuery.Step = step

	matrix, err := ExecuteQueryRange(ctx, client, rangeQuery)
	if err != nil {
		return fmt.Errorf("failed to execute clock class range query: %w", err)
	}

	if len(matrix) == 0 {
		return fmt.Errorf("clock class range query returned no samples between %s and %s",
			rangeQuery.Start, rangeQuery.End)
	}

	for _, series := range matrix {
		deviationIndex := slices.IndexFunc(series.Values, func(s model.SamplePair) bool {
			return convertSampleValueToInt64(s.Value) != int64(expected)
		})
		if deviationIndex == -1 {
			continue
		}

		deviation := series.Values[deviationIndex]

		return fmt.Errorf("clock class deviated from %d to %d at %s (series %s)",
			expected, convertSampleValueToInt64(deviation.Value), deviation.Timestamp.Time(), series.Metric)
	}

	return nil
}
