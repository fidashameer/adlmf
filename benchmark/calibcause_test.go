package benchmark

import (
	"fmt"
	"testing"

	"adlmf/recovery"
)

// H9 shows calibration losing on its own target. The suspected cause is that
// fitting the average cost drives the blast-radius weight to zero, so victim
// choice stops discriminating on blast radius and collapses onto fairness.
// Picking worse victims then raises the very cost being regressed.
//
// Test: freeze the weights at what the learner converges to, with no learning
// at all. If that alone reproduces the loss, the weights are the cause and the
// learning dynamics are incidental.
func TestCalibrationFailureCause(t *testing.T) {
	run := func(mk func() recovery.Policy) (float64, float64, float64) {
		var comp, disr, lost []float64
		for trial := 0; trial < 20; trial++ {
			cfg := baseConfig(int64(12000 + trial))
			cfg.CriticalitySkew = 10
			cfg.LocksPerTxn = 4
			r := NewSim(cfg, mk()).Run()
			comp = append(comp, float64(r.Completed))
			disr = append(disr, Mean(r.Disrupted))
			lost = append(lost, Mean(r.WorkLost))
		}
		return Mean(comp), Mean(disr), Mean(lost)
	}

	cases := []struct {
		name string
		mk   func() recovery.Policy
	}{
		{"alpha=0, default weights", func() recovery.Policy { return recovery.NewAdaptivePolicy(0) }},
		{"alpha=0, blast weight frozen at 0", func() recovery.Policy {
			p := recovery.NewAdaptivePolicy(0)
			p.W.BlastRadius = 0
			return p
		}},
		{"alpha=0.08, learning", func() recovery.Policy { return recovery.NewAdaptivePolicy(0.08) }},
	}
	for _, c := range cases {
		comp, disr, lost := run(c.mk)
		fmt.Printf("%-36s completed %6.0f  disrupted %7.3f  work lost %.3f\n", c.name, comp, disr, lost)
	}
}
