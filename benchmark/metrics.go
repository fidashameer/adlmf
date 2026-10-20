package benchmark

import (
	"math"
	"sort"
)

// Mean returns the arithmetic mean, or 0 for an empty sample.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Percentile returns the p-th percentile (0..100) using nearest-rank.
//
// P95 is reported alongside the mean for recovery time because a policy that
// is usually fast but occasionally very slow behaves quite differently in
// practice from one that is consistently medium-speed, and the mean alone
// hides that.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// StdDev returns the sample standard deviation.
func StdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := Mean(xs)
	var ss float64
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// CI95 returns the half-width of a 95% confidence interval on the mean.
//
// Using 1.96 is a normal approximation. With the trial counts here (30+) that
// is reasonable; it is quoted rather than hidden so the assumption is visible.
func CI95(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	return 1.96 * StdDev(xs) / math.Sqrt(float64(len(xs)))
}

// Summary bundles the statistics reported for every metric.
type Summary struct {
	N    int
	Mean float64
	P95  float64
	CI   float64
}

// Summarise computes the standard set.
func Summarise(xs []float64) Summary {
	return Summary{N: len(xs), Mean: Mean(xs), P95: Percentile(xs, 95), CI: CI95(xs)}
}

// SeparatedCI reports whether two means differ by more than their confidence
// intervals, i.e. whether the difference survives the noise in the trials.
//
// This is a deliberately conservative check, not a t-test. It is used so that
// the write-up can say "separated at 95%" only when the intervals genuinely do
// not overlap, rather than calling any difference a result.
func SeparatedCI(a, b Summary) bool {
	return math.Abs(a.Mean-b.Mean) > (a.CI + b.CI)
}
