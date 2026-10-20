// Command experiments runs every hypothesis from Section 8 of the report and
// prints the results.
//
//	go run ./cmd/experiments
package main

import (
	"flag"
	"fmt"
	"os"

	"adlmf/benchmark"
)

func main() {
	trials := flag.Int("trials", 30, "trials per configuration")
	only := flag.String("only", "", "run one hypothesis: h1..h9")
	flag.Parse()

	fmt.Println("Adaptive Distributed Lock Management Framework")
	fmt.Printf("Section 8 experiments, %d trials per configuration\n", *trials)
	fmt.Println("Each run is seeded, so these numbers reproduce exactly.")
	fmt.Println()

	run := map[string]func(int) string{
		"h1":  benchmark.RunDisruptionExperiment,
		"h2":  benchmark.RunContentionSweep,
		"h3":  benchmark.RunOverheadExperiment,
		"h4":  benchmark.RunCrashExperiment,
		"h5":  benchmark.RunPillar1Experiment,
		"h6":  benchmark.RunExplanationOverheadExperiment,
		"h7":  benchmark.RunAblation,
		"h8":  benchmark.RunPartitionExperiment,
		"h9":  benchmark.RunCalibrationExperiment,
		"h10": benchmark.RunFrequencyExperiment,
	}
	order := []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8", "h9", "h10"}

	if *only != "" {
		f, ok := run[*only]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown hypothesis %q\n", *only)
			os.Exit(1)
		}
		fmt.Println(f(*trials))
		return
	}

	for _, k := range order {
		fmt.Println(run[k](*trials))
	}
}
