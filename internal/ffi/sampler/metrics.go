package sampler

import "slices"

const (
	metricSMPipeTensorCyclesActive   = "sm__pipe_tensor_cycles_active.avg.pct_of_peak_sustained_elapsed"
	metricSMPipeFmaCyclesActive      = "sm__pipe_fma_cycles_active.avg.pct_of_peak_sustained_elapsed"
	metricSMPipeAluCyclesActive      = "sm__pipe_alu_cycles_active.avg.pct_of_peak_sustained_elapsed"
	metricSMInstExecutedPipeLsu      = "sm__inst_executed_pipe_lsu.avg.pct_of_peak_sustained_elapsed"
	metricSMIssueActive              = "sm__issue_active.avg.pct_of_peak_sustained_elapsed"
	metricSMCyclesActive             = "sm__cycles_active.avg.pct_of_peak_sustained_elapsed"
	metricDramThroughput             = "dram__throughput.avg.pct_of_peak_sustained_elapsed"
	metricL1texDataPipeLsuWavefronts = "l1tex__data_pipe_lsu_wavefronts.avg.pct_of_peak_sustained_elapsed"
)

// PipeFamily identifies whether a pipe contributes to Compute SOL or Memory SOL.
type PipeFamily string

const (
	PipeFamilyCompute PipeFamily = "compute"
	PipeFamilyMemory  PipeFamily = "memory"
)

// Pipe is a friendly-named sub-metric whose pct_of_peak_sustained_elapsed value
// contributes to a SOL roll-up (max-of-family).
type Pipe struct {
	Name   string     // short label used in OTEL attrs, e.g. "tensor"
	Metric string     // NVPerf metric name
	Family PipeFamily // "compute" or "memory"
}

var (
	// Pipes is the full breakdown that drives Compute SOL = max(compute pipes)
	// and Memory SOL = max(memory pipes).
	Pipes = []Pipe{
		{Name: "tensor", Metric: metricSMPipeTensorCyclesActive, Family: PipeFamilyCompute},
		{Name: "fma", Metric: metricSMPipeFmaCyclesActive, Family: PipeFamilyCompute},
		{Name: "alu", Metric: metricSMPipeAluCyclesActive, Family: PipeFamilyCompute},
		{Name: "lsu_inst", Metric: metricSMInstExecutedPipeLsu, Family: PipeFamilyCompute},
		{Name: "issue", Metric: metricSMIssueActive, Family: PipeFamilyCompute},
		{Name: "dram", Metric: metricDramThroughput, Family: PipeFamilyMemory},
		{Name: "l1tex", Metric: metricL1texDataPipeLsuWavefronts, Family: PipeFamilyMemory},
	}

	smSubPipeMetrics = pipeMetricsFor(PipeFamilyCompute)
	memMetrics       = pipeMetricsFor(PipeFamilyMemory)
	dcgmMetrics      = []string{metricSMCyclesActive}

	DefaultMetrics = slices.Concat(smSubPipeMetrics, memMetrics, dcgmMetrics)
)

func pipeMetricsFor(family PipeFamily) []string {
	var out []string
	for _, p := range Pipes {
		if p.Family == family {
			out = append(out, p.Metric)
		}
	}
	return out
}
