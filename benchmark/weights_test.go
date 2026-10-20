package benchmark

import (
	"fmt"
	"testing"

	"adlmf/recovery"
)

// H9 showed alpha=0.08 and alpha=0.30 producing identical disruption to three
// decimals. Either the weights converge to the same point regardless of the
// learning rate, or they run away and get clamped. Which one it is decides
// whether "self-calibrating" is a fair description of the mechanism.
func TestWeightTrajectory(t *testing.T) {
	for _, alpha := range []float64{0.08, 0.30} {
		cfg := baseConfig(7)
		cfg.CriticalitySkew = 10
		p := recovery.NewAdaptivePolicy(alpha)
		_ = NewSim(cfg, p).Run()
		w := p.W
		fmt.Printf("alpha=%.2f final weights: blast=%.4f rollback=%.4f partition=%.4f fairness=%.4f\n",
			alpha, w.BlastRadius, w.RollbackCost, w.Partition, w.Fairness)
	}
	d := recovery.DefaultWeights()
	fmt.Printf("defaults            : blast=%.4f rollback=%.4f partition=%.4f fairness=%.4f\n",
		d.BlastRadius, d.RollbackCost, d.Partition, d.Fairness)
}
