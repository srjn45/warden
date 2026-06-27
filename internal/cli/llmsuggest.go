package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
)

// llmCandidate is one local model warden can recommend for the orchestrator
// (`wd repl`). The orchestrator *conducts* — it turns intent into warden tool
// calls and never writes code — so the capability that matters is reliable
// tool/function calling, not coding benchmarks. RAMGB is the practical memory to
// run the model well (≈Q4 weights + KV cache + runtime overhead), the figure the
// recommendation engine compares against detected memory.
type llmCandidate struct {
	Name   string  `json:"name"`   // Ollama tag (what you put in local_llm_model)
	Params string  `json:"params"` // human size label
	RAMGB  float64 `json:"ram_gb"` // practical memory to run it well
	Family string  `json:"family"`
	// Score is conductor suitability (1–10): how reliable the model is at the
	// orchestrator's job — tool/function calling and instruction-following — NOT
	// coding or raw size. The recommendation picks the best-scoring model that
	// fits, so a leaner agentic model beats a bigger coding-tuned one.
	Score int    `json:"score"`
	Note  string `json:"note"` // why it fits the conductor role
}

// llmCatalog is warden's curated shortlist of local models for the orchestrator,
// ordered by memory footprint. It is intentionally tool-calling-forward (general
// instruct / agentic models, MoE where it buys efficiency) rather than
// coding-tuned: the conductor routes tool calls, it does not implement. RAM
// figures are approximate Q4 footprints.
//
// Scores are calibrated against the Berkeley Function-Calling Leaderboard (BFCL
// v4, last updated 2025-12-16; gorilla.cs.berkeley.edu/leaderboard.html),
// weighted toward the MULTI-TURN subcategory because the orchestrator runs a
// multi-turn tool-call loop. Two findings drive the ranking: (1) Qwen3 beats
// Qwen2.5 at equal size and dominates the open-model board; (2) the curve is
// steep at the small end — multi-turn accuracy falls off a cliff below ~8B
// (Qwen3-32B ≈ 47% vs Qwen3-4B ≈ 16% multi-turn), so small models score low.
// BFCL doesn't publish a row for every exact Ollama tag, so sizes between
// measured anchors are interpolated within their family. Leaderboards move
// monthly — re-check the source before trusting a close call.
var llmCatalog = []llmCandidate{
	{"qwen2.5:3b", "3B", 3, "Qwen2.5", 2, "last-resort floor; weak at multi-turn tool calls"},
	{"qwen3:4b", "4B", 4, "Qwen3", 3, "runs on little memory, but small-model multi-turn is shaky"},
	{"qwen2.5:7b", "7B", 6, "Qwen2.5", 4, "usable single-turn tool calling; older family"},
	{"qwen3:8b", "8B", 7, "Qwen3", 6, "solid Qwen3 tool use; the practical small-machine floor"},
	{"qwen3:14b", "14B", 10, "Qwen3", 7, "sweet spot for tool routing on a working dev machine"},
	{"mistral-small3.2", "24B", 15, "Mistral", 7, "strong instruction-following + function calling"},
	{"gpt-oss:20b", "20B MoE (~3.6B active)", 16, "gpt-oss", 8, "strong agentic tool use, fast despite size (MoE)"},
	{"qwen3:30b-a3b", "30B MoE (3B active)", 20, "Qwen3", 9, "best locally-runnable conductor; near-flagship BFCL, fast MoE"},
	{"qwen2.5-coder:32b", "32B", 22, "Qwen2.5-Coder", 4, "warden's legacy default; coding-tuned + older family, weak multi-turn for its size"},
	{"gpt-oss:120b", "120B MoE", 65, "gpt-oss", 10, "server-class; closest to flagship tool-calling, dedicated hosts only"},
}

// fitStatus is how a candidate relates to the detected memory.
type fitStatus int

const (
	fitTooLarge  fitStatus = iota // won't fit the machine at all
	fitFreeFirst                  // fits total RAM, but not what's free right now
	fitNow                        // runnable right now within free memory
)

func (s fitStatus) label() string {
	switch s {
	case fitNow:
		return "fits now"
	case fitFreeFirst:
		return "free memory first"
	default:
		return "too large"
	}
}

// llmSuggestion is a catalog entry scored against the machine's memory.
type llmSuggestion struct {
	llmCandidate
	Status      fitStatus `json:"-"`
	StatusLabel string    `json:"status"`
	Recommended bool      `json:"recommended"`
}

// suggestModels scores every catalog entry against the machine's total and
// currently-free memory. totalGB bounds what could ever run (after closing other
// apps); freeGB bounds what can run right now without forcing the machine to
// swap. It reserves headroom on both axes so a recommended model never claims the
// last gigabyte — the orchestrator model runs persistently alongside the
// operator's real workload (Docker, DBs, IDE, Claude sessions, the daemon).
func suggestModels(totalGB, freeGB float64) []llmSuggestion {
	sysReserve := reserveGB(totalGB) // leave this much for the OS + apps
	comfort := reserveGB(freeGB)     // and this much slack within free memory

	out := make([]llmSuggestion, 0, len(llmCatalog))
	comfortable := -1 // index of the best-scoring model with comfortable headroom now
	for i, c := range llmCatalog {
		s := llmSuggestion{llmCandidate: c}
		switch {
		case c.RAMGB <= freeGB:
			s.Status = fitNow
			if c.RAMGB <= freeGB-comfort {
				comfortable = better(llmCatalog, comfortable, i)
			}
		case c.RAMGB <= totalGB-sysReserve:
			s.Status = fitFreeFirst
		default:
			s.Status = fitTooLarge
		}
		s.StatusLabel = s.Status.label()
		out = append(out, s)
	}
	if r := pickRecommended(out, comfortable); r >= 0 {
		out[r].Recommended = true
	}
	return out
}

// pickRecommended chooses the model to star, in preference order: the
// best-scoring model that runs comfortably now; else the best-scoring model that
// fits now at all; else the best-scoring model that fits after freeing memory;
// else the smallest model in the catalog, so even a memory-starved machine gets
// pointed at the floor.
func pickRecommended(s []llmSuggestion, comfortable int) int {
	if comfortable >= 0 {
		return comfortable
	}
	if i := bestWithStatus(s, fitNow); i >= 0 {
		return i
	}
	if i := bestWithStatus(s, fitFreeFirst); i >= 0 {
		return i
	}
	if len(s) > 0 {
		return 0 // catalog is ascending — the smallest model is the floor
	}
	return -1
}

// better returns whichever catalog index is the stronger conductor pick: higher
// Score wins; ties break toward less memory (the leaner model). cur may be -1.
func better(cat []llmCandidate, cur, cand int) int {
	if cur < 0 {
		return cand
	}
	switch {
	case cat[cand].Score != cat[cur].Score:
		if cat[cand].Score > cat[cur].Score {
			return cand
		}
		return cur
	case cat[cand].RAMGB < cat[cur].RAMGB:
		return cand
	default:
		return cur
	}
}

// bestWithStatus returns the index of the strongest-scoring candidate with the
// given status, or -1.
func bestWithStatus(s []llmSuggestion, st fitStatus) int {
	best := -1
	cat := make([]llmCandidate, len(s))
	for i := range s {
		cat[i] = s[i].llmCandidate
	}
	for i := range s {
		if s[i].Status == st {
			best = better(cat, best, i)
		}
	}
	return best
}

// reserveGB is the memory headroom to keep free: a flat floor plus a fraction,
// so big machines reserve proportionally more (a 64 GB dev box runs far more than
// a model).
func reserveGB(gb float64) float64 {
	r := gb * 0.2
	if r < 2 {
		r = 2
	}
	return r
}

// ---- available-memory detection (free + reclaimable), best-effort -----------

// availMemGB probes the free memory of the SAME pool that bounds the model,
// identified by source (the memProbe label from total detection): free VRAM for
// an NVIDIA GPU, the reclaimable unified pool on Apple Silicon, or Linux
// MemAvailable for a CPU/system-RAM machine. Measuring the wrong pool (e.g.
// system RAM on a small-VRAM box) would wildly overstate what a model can claim,
// so the source must match. run and procAvail are injected for testability.
func availMemGB(run func(name string, args ...string) ([]byte, error), source string, procAvail func() (float64, bool)) (float64, bool) {
	switch source {
	case "NVIDIA VRAM":
		if out, err := run("nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits"); err == nil {
			if gb, ok := maxGPUMiBToGB(string(out)); ok {
				return gb, true
			}
		}
		return 0, false
	case "unified memory":
		if out, err := run("vm_stat"); err == nil {
			return vmStatAvailGB(string(out))
		}
		return 0, false
	default: // system RAM (no GPU) — Linux MemAvailable
		return procAvail()
	}
}

// maxGPUMiBToGB parses one MiB-per-line nvidia-smi column and returns the largest
// GPU's value in GB (matching how detectMemoryGB picks the largest GPU for total).
func maxGPUMiBToGB(out string) (float64, bool) {
	var maxMiB float64
	found := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if v, err := strconv.ParseFloat(strings.TrimSpace(sc.Text()), 64); err == nil {
			found = true
			if v > maxMiB {
				maxMiB = v
			}
		}
	}
	if !found {
		return 0, false
	}
	return maxMiB / 1024, true
}

// procMemAvailableGB reads MemAvailable from /proc/meminfo (Linux). MemAvailable
// is the kernel's own estimate of memory obtainable without swapping, so it is
// exactly the "free for a new workload" figure we want.
func procMemAvailableGB() (float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "MemAvailable:") {
			continue
		}
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 {
			if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
				return kb / 1024 / 1024, true // kB → GB
			}
		}
	}
	return 0, false
}

// vmStatAvailGB sums the reclaimable page buckets from `vm_stat` output (free +
// inactive + speculative + purgeable) into GB. On macOS these are the pages the
// OS can hand to a new process without paging anything out.
func vmStatAvailGB(raw string) (float64, bool) {
	pageSize := int64(4096)
	counts := map[string]int64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := strings.TrimSuffix(strings.TrimSpace(line[i+len("page size of "):]), " bytes)")
				if ff := strings.Fields(rest); len(ff) > 0 {
					if n, err := strconv.ParseInt(ff[0], 10, 64); err == nil {
						pageSize = n
					}
				}
			}
			continue
		}
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSuffix(strings.TrimSpace(line[i+1:]), ".")
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			counts[key] = n
		}
	}
	var pages int64
	found := false
	for _, k := range []string{"Pages free", "Pages inactive", "Pages speculative", "Pages purgeable"} {
		if n, ok := counts[k]; ok {
			pages += n
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return float64(pages*pageSize) / (1 << 30), true
}

// avgAvail samples available memory n times and averages, smoothing momentary
// spikes into a representative free-memory figure. sleep is injected so tests run
// instantly. Samples that fail to read are skipped; if none succeed it returns
// not-ok.
func avgAvail(sample func() (float64, bool), n int, sleep func()) (float64, bool) {
	if n < 1 {
		n = 1
	}
	var sum float64
	got := 0
	for i := 0; i < n; i++ {
		if i > 0 {
			sleep()
		}
		if v, ok := sample(); ok {
			sum += v
			got++
		}
	}
	if got == 0 {
		return 0, false
	}
	return sum / float64(got), true
}

// ---- command ----------------------------------------------------------------

func newLLMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llm",
		Short: "Local-LLM helpers for the REPL (wd repl)",
	}
	cmd.AddCommand(newLLMSuggestCmd())
	return cmd
}

func newLLMSuggestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "Recommend local models for the REPL, sized to this machine's memory",
		Long: `Suggest local LLM models for warden's REPL (wd repl), ranked against
this machine's memory.

warden auto-detects two figures: total memory (GPU VRAM, Apple unified memory, or
system RAM — whichever bounds a usable model) and average free memory (sampled a
few times to smooth out spikes). Each candidate is then marked:

  fits now           runnable right now within free memory
  free memory first  fits the machine, but you'd need to close apps first
  too large          won't fit this machine

Models are scored by suitability for the conductor role — reliable tool/function
calling, not coding or raw size. The recommendation (★) is the best-scoring model
that runs comfortably now while leaving headroom for your real workload (Docker,
DBs, IDE, Claude sessions, the warden daemon). warden only ever recommends — you
set local_llm_model yourself.`,
		Args: cobra.NoArgs,
		RunE: runLLMSuggest,
	}
	cmd.Flags().Int("samples", 5, "free-memory samples to average")
	cmd.Flags().Float64("total-gb", 0, "override detected total memory (GB)")
	cmd.Flags().Float64("free-gb", 0, "override detected free memory (GB)")
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// llmSuggestReport is the structured payload behind `wd llm suggest --json`.
type llmSuggestReport struct {
	TotalGB     float64         `json:"total_gb"`
	FreeGB      float64         `json:"free_gb"`
	TotalSource string          `json:"total_source"`
	Detected    bool            `json:"detected"`
	Configured  string          `json:"configured_model,omitempty"`
	Suggestions []llmSuggestion `json:"suggestions"`
}

func runLLMSuggest(cmd *cobra.Command, _ []string) error {
	cfg := config.Load(configPathFor(cmd))
	samples, _ := cmd.Flags().GetInt("samples")
	totalOverride, _ := cmd.Flags().GetFloat64("total-gb")
	freeOverride, _ := cmd.Flags().GetFloat64("free-gb")

	report := llmSuggestReport{Configured: strings.TrimSpace(cfg.LocalLLM.Model)}

	total := detectMemoryGB(runCmdOutput, goosName(), systemRAMGB)
	report.TotalGB, report.TotalSource, report.Detected = total.gb, total.source, total.ok
	if totalOverride > 0 {
		report.TotalGB, report.TotalSource, report.Detected = totalOverride, "override", true
	}

	if freeOverride > 0 {
		report.FreeGB = freeOverride
	} else if free, ok := detectAvgFreeMem(total.source, samples); ok {
		report.FreeGB = free
	} else if report.Detected {
		report.FreeGB = report.TotalGB // unknown free → assume the whole machine
	}
	// Free can never exceed total of the same pool (a momentary sampling artifact,
	// or an override); clamp so the fit logic stays coherent.
	if report.Detected && report.FreeGB > report.TotalGB {
		report.FreeGB = report.TotalGB
	}

	if !report.Detected && totalOverride == 0 {
		// Couldn't size the machine at all; recommend the conservative floor and say so.
		report.Suggestions = suggestModels(4, 4)
	} else {
		report.Suggestions = suggestModels(report.TotalGB, report.FreeGB)
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return printJSON(cmd.OutOrStdout(), report)
	}
	printLLMSuggest(cmd, report)
	return nil
}

// detectAvgFreeMem samples the free memory of the bounding pool (named by
// source) `samples` times and averages it.
func detectAvgFreeMem(source string, samples int) (float64, bool) {
	sample := func() (float64, bool) {
		return availMemGB(runCmdOutput, source, procMemAvailableGB)
	}
	return avgAvail(sample, samples, func() { time.Sleep(150 * time.Millisecond) })
}

// runCmdOutput shells out and returns combined stdout, the form the memory
// probes expect (same shape doctor uses for its hardware detection).
func runCmdOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func goosName() string { return runtime.GOOS }

func printLLMSuggest(cmd *cobra.Command, r llmSuggestReport) {
	out := cmd.OutOrStdout()
	if r.Detected {
		fmt.Fprintf(out, "Detected: %.0f GB %s total · ~%.0f GB free (avg)\n", r.TotalGB, r.TotalSource, r.FreeGB)
	} else {
		fmt.Fprintln(out, "Detected: hardware undetected — showing conservative suggestions for ~4 GB")
	}
	if r.Configured != "" {
		fmt.Fprintf(out, "Configured: local_llm_model = %s\n", r.Configured)
	}
	fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\tMODEL\tSIZE\t~RAM\tFIT\tNOTES")
	for _, s := range r.Suggestions {
		mark := " "
		switch {
		case s.Recommended:
			mark = "★"
		case s.Name == r.Configured:
			mark = "•"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f GB\t%s\t%s\n",
			mark, s.Name, s.Params, s.RAMGB, s.StatusLabel, s.Note)
	}
	_ = tw.Flush()

	fmt.Fprintln(out, "\n★ recommended · • currently configured")
	if rec, ok := recommended(r.Suggestions); ok {
		switch {
		case rec.Status == fitNow && rec.Name != r.Configured:
			fmt.Fprintf(out, "Set it: edit local_llm_model to %s in your config file (`wd config path`), then restart the daemon.\n", rec.Name)
		case rec.Status == fitFreeFirst:
			fmt.Fprintf(out, "Nothing fits the free memory right now. %s is the best fit once you free some — close apps, or run it on CPU/system RAM.\n", rec.Name)
		case rec.Status == fitTooLarge:
			fmt.Fprintf(out, "This machine is below the comfortable range for these models. %s is the lightest option; expect it to be tight.\n", rec.Name)
		}
	}
	fmt.Fprintln(out, "Figures are approximate Q4 footprints; warden recommends, you decide.")
}

func recommended(s []llmSuggestion) (llmSuggestion, bool) {
	for _, c := range s {
		if c.Recommended {
			return c, true
		}
	}
	return llmSuggestion{}, false
}

// sortByRAM keeps the catalog ascending; defensive in case the literal drifts.
func init() {
	sort.SliceStable(llmCatalog, func(i, j int) bool { return llmCatalog[i].RAMGB < llmCatalog[j].RAMGB })
}
