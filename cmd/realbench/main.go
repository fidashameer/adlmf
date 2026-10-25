// Command realbench runs the end-to-end harness repeatedly against a real etcd
// and aggregates the results, so the policy comparisons that Section VI reports
// from the simulator can also be run against real operating-system processes.
//
// The simulator and this command answer different questions. The simulator puts
// every policy through an identical seeded workload, which is the only way to
// attribute a difference to the policy rather than to timing. This command gives
// up that control in exchange for realism: real processes, real etcd, real
// scheduling jitter. Differences here have to survive variance that the
// simulator deliberately removes, so the trial counts need to be higher and
// small effects are not expected to show.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type run struct {
	completed  float64
	deadlocks  float64
	violations float64
	// maxAborts is how many times the most-aborted service was sacrificed in
	// a run. It is the direct measure of starvation, independent of whether
	// starvation turns out to cost any throughput.
	maxAborts float64
}

type cell struct {
	policy string
	runs   []run
}

func main() {
	e2eBin := flag.String("e2e", "", "path to a prebuilt e2e binary (required)")
	stubBin := flag.String("stub", "", "path to a prebuilt stubservice binary (required)")
	endpoint := flag.String("etcd", "http://127.0.0.1:2379", "etcd endpoint")
	trials := flag.Int("trials", 10, "trials per policy per configuration")
	runFor := flag.Duration("for", 25*time.Second, "duration of each trial")
	policies := flag.String("policies", "timeout,cost-only,youngest,blast-only,adaptive",
		"comma-separated policies to compare")
	configs := flag.String("configs", "6x3,20x3",
		"comma-separated services x locks configurations, e.g. 6x3,20x3")
	varyLocks := flag.Bool("vary-locks", false, "give services differing lock counts")
	raw := flag.String("raw", "", "append one line per completed trial to this file, so an "+
		"interrupted run can be resumed and aggregated later")
	reportOnly := flag.String("report", "", "skip running and aggregate an existing raw file")
	flag.Parse()

	if *reportOnly != "" {
		if err := reportFile(*reportOnly); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return
	}

	if *e2eBin == "" || *stubBin == "" {
		fmt.Println("both -e2e and -stub are required; build them with:")
		fmt.Println("  go build -o /tmp/e2e ./cmd/e2e && go build -o /tmp/stubservice ./cmd/stubservice")
		os.Exit(1)
	}

	pols := strings.Split(*policies, ",")
	cfgs := strings.Split(*configs, ",")

	total := len(pols) * len(cfgs) * *trials
	fmt.Printf("Real-harness policy comparison\n")
	fmt.Printf("%d trials per cell, %s each, %d cells, about %s of running\n\n",
		*trials, *runFor, len(pols)*len(cfgs),
		(time.Duration(total) * *runFor).Round(time.Minute))

	done := 0
	for _, cfg := range cfgs {
		svcs, locks, err := parseConfig(cfg)
		if err != nil {
			fmt.Printf("bad config %q: %v\n", cfg, err)
			os.Exit(1)
		}

		fmt.Printf("%d services over %d locks", svcs, locks)
		if *varyLocks {
			fmt.Printf(", differing lock counts")
		}
		fmt.Printf("\n")

		var cells []cell
		for _, pol := range pols {
			c := cell{policy: pol}
			for t := 0; t < *trials; t++ {
				r, err := once(*e2eBin, *stubBin, *endpoint, pol, svcs, locks, *runFor, *varyLocks)
				if err != nil {
					fmt.Printf("    trial failed: %v\n", err)
					continue
				}
				c.runs = append(c.runs, r)
				if *raw != "" {
					appendRaw(*raw, cfg, pol, r)
				}
				done++
				fmt.Fprintf(os.Stderr, "\r  %d/%d trials", done, total)
			}
			cells = append(cells, c)
		}
		fmt.Fprintf(os.Stderr, "\r                    \r")
		report(cells)
	}
}

func parseConfig(s string) (svcs, locks int, err error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected <services>x<locks>")
	}
	if svcs, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, err
	}
	locks, err = strconv.Atoi(parts[1])
	return svcs, locks, err
}

// once runs a single trial and returns the counters from its RESULT line.
func once(e2eBin, stubBin, endpoint, policy string, svcs, locks int,
	d time.Duration, vary bool) (run, error) {

	args := []string{
		"-etcd", endpoint, "-stub", stubBin, "-quiet",
		"-policy", policy,
		"-services", strconv.Itoa(svcs),
		"-locks", strconv.Itoa(locks),
		"-for", d.String(),
	}
	if vary {
		args = append(args, "-vary-locks")
	}

	out, err := exec.Command(e2eBin, args...).Output()
	if err != nil {
		return run{}, err
	}

	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "RESULT ") {
			continue
		}
		var r run
		for _, kv := range strings.Fields(strings.TrimPrefix(line, "RESULT ")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, convErr := strconv.ParseFloat(v, 64)
			if convErr != nil {
				continue
			}
			switch k {
			case "completed":
				r.completed = n
			case "deadlocks":
				r.deadlocks = n
			case "violations":
				r.violations = n
			case "maxaborts":
				r.maxAborts = n
			}
		}
		return r, nil
	}
	return run{}, fmt.Errorf("no RESULT line in output")
}

func report(cells []cell) {
	fmt.Printf("    %-12s %18s %18s %16s %14s %s\n",
		"policy", "completed", "deadlocks", "per 1k completed", "worst-starved", "excl")
	for _, c := range cells {
		if len(c.runs) == 0 {
			continue
		}
		comp := field(c.runs, func(r run) float64 { return r.completed })
		dead := field(c.runs, func(r run) float64 { return r.deadlocks })
		var ratios []float64
		var viol float64
		for _, r := range c.runs {
			viol += r.violations
			if r.completed > 0 {
				ratios = append(ratios, 1000*r.deadlocks/r.completed)
			}
		}
		rm, rc := meanCI(ratios)

		cm, cc := meanCI(comp)
		dm, dc := meanCI(dead)
		am, ac := meanCI(field(c.runs, func(r run) float64 { return r.maxAborts }))
		fmt.Printf("    %-12s %8.1f +/- %-6.1f %8.1f +/- %-6.1f %7.1f +/- %-6.1f %6.1f +/- %-5.1f %4.0f\n",
			c.policy, cm, cc, dm, dc, rm, rc, am, ac, viol)
	}

	// State plainly whether the best and second-best completed counts are
	// actually distinguishable, rather than leaving it to be eyeballed.
	type s struct {
		name     string
		mean, ci float64
	}
	var ranked []s
	for _, c := range cells {
		if len(c.runs) == 0 {
			continue
		}
		m, ci := meanCI(field(c.runs, func(r run) float64 { return r.completed }))
		ranked = append(ranked, s{c.policy, m, ci})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].mean > ranked[j].mean })
	if len(ranked) >= 2 {
		a, b := ranked[0], ranked[1]
		if a.mean-a.ci > b.mean+b.ci {
			fmt.Printf("    best on completed: %s over %s, intervals disjoint\n", a.name, b.name)
		} else {
			fmt.Printf("    best on completed: %s over %s, but the intervals OVERLAP, no win claimed\n",
				a.name, b.name)
		}
	}
	fmt.Println()
}

func field(rs []run, f func(run) float64) []float64 {
	out := make([]float64, 0, len(rs))
	for _, r := range rs {
		out = append(out, f(r))
	}
	return out
}

// meanCI returns the mean and the half-width of a 95% confidence interval.
func meanCI(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / float64(len(xs)-1))
	return mean, 1.96 * sd / math.Sqrt(float64(len(xs)))
}

// appendRaw records a single trial so that a run killed part way through is not
// wasted. The format is deliberately flat: config, policy, then the counters.
func appendRaw(path, cfg, pol string, r run) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%.0f\t%.0f\t%.0f\t%.0f\n", cfg, pol, r.completed, r.deadlocks, r.violations, r.maxAborts)
}

// reportFile aggregates a raw file written by earlier runs, grouping by
// configuration and preserving the order each policy first appeared in.
func reportFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	byCfg := map[string][]cell{}
	var cfgOrder []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		cfg, pol := f[0], f[1]
		var r run
		r.completed, _ = strconv.ParseFloat(f[2], 64)
		r.deadlocks, _ = strconv.ParseFloat(f[3], 64)
		r.violations, _ = strconv.ParseFloat(f[4], 64)
		if len(f) > 5 {
			r.maxAborts, _ = strconv.ParseFloat(f[5], 64)
		}

		if _, seen := byCfg[cfg]; !seen {
			cfgOrder = append(cfgOrder, cfg)
		}
		cells := byCfg[cfg]
		found := false
		for i := range cells {
			if cells[i].policy == pol {
				cells[i].runs = append(cells[i].runs, r)
				found = true
				break
			}
		}
		if !found {
			cells = append(cells, cell{policy: pol, runs: []run{r}})
		}
		byCfg[cfg] = cells
	}

	for _, cfg := range cfgOrder {
		n := 0
		for _, c := range byCfg[cfg] {
			n += len(c.runs)
		}
		fmt.Printf("%s  (%d trials)\n", cfg, n)
		report(byCfg[cfg])
	}
	return nil
}
