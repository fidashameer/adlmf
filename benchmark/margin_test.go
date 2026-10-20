package benchmark

import (
	"fmt"
	"testing"

	"adlmf/recovery"
)

// Measures, for every decision where at least one cycle member is
// partition-suspect and at least one is not, the score gap between the best
// suspect and the best reachable candidate. The weighted term contributes
// W.Partition (=2.0). If the gap frequently sits below that, the weighted
// form is winning only by a thin margin and would flip at a different
// workload; the tier does not depend on any margin.
func TestPartitionMargin(t *testing.T) {
	cfg := baseConfig(1)
	cfg.PartitionAt = 1000
	cfg.PartitionFor = 1500
	var gaps []float64
	mixed := 0
	for trial := 0; trial < 20; trial++ {
		c := cfg
		c.Seed = int64(trial + 1)
		p := recovery.NewAdaptivePolicy(0.08)
		p.DisablePartitionTier = true
		p.AuditGap = func(suspectBest, reachBest float64) {
			mixed++
			gaps = append(gaps, suspectBest-reachBest)
		}
		_ = NewSim(c, p).Run()
	}
	if mixed == 0 {
		t.Fatalf("no decision ever had both a suspect and a reachable candidate: H8 proves nothing about the term")
	}
	below := 0
	for _, g := range gaps {
		if g < 2.0 {
			below++
		}
	}
	fmt.Printf("mixed-tier decisions: %d\n", mixed)
	fmt.Printf("gap (suspect-best minus reachable-best, excluding the 2.0 term):\n")
	fmt.Printf("  mean %.2f  p5 %.2f  p50 %.2f\n", Mean(gaps), Percentile(gaps, 5), Percentile(gaps, 50))
	fmt.Printf("  decisions where the gap alone was under 2.0 (so the weight was load-bearing): %d / %d\n", below, len(gaps))
}
