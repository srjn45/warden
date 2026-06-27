package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/config"
)

// recommendModel maps detected accelerator/host memory (GB) to a recommended
// local_llm_model from the Qwen2.5-Coder family, per the orchestrator design's
// hardware-aware table. warden recommends; it never auto-sets the model.
func recommendModel(gb float64) string {
	switch {
	case gb >= 20:
		return "qwen2.5-coder:32b"
	case gb >= 10:
		return "qwen2.5-coder:14b"
	case gb >= 6:
		return "qwen2.5-coder:7b"
	case gb >= 3.5:
		return "qwen2.5-coder:3b"
	default:
		return "qwen2.5-coder:1.5b"
	}
}

// memProbe is the detected memory that bounds local-LLM model size.
type memProbe struct {
	gb     float64
	source string // human label, e.g. "NVIDIA VRAM", "unified memory", "system RAM"
	ok     bool
}

// detectMemoryGB is a best-effort, platform-specific probe of the memory that
// bounds a usable local model: GPU VRAM first (nvidia-smi), then Apple unified
// memory (sysctl), then system RAM. run, goos, and sysRAM are injected so the
// probe is fully testable. Any failure falls through to the next source.
func detectMemoryGB(run func(name string, args ...string) ([]byte, error), goos string, sysRAM func() (float64, bool)) memProbe {
	// 1. NVIDIA VRAM — one MiB total per GPU line; take the largest GPU.
	if out, err := run("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits"); err == nil {
		var maxMiB float64
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			if v, err := strconv.ParseFloat(strings.TrimSpace(sc.Text()), 64); err == nil && v > maxMiB {
				maxMiB = v
			}
		}
		if maxMiB > 0 {
			return memProbe{gb: maxMiB / 1024, source: "NVIDIA VRAM", ok: true}
		}
	}
	// 2. Apple Silicon unified memory (shared CPU/GPU pool).
	if goos == "darwin" {
		if out, err := run("sysctl", "-n", "hw.memsize"); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil && v > 0 {
				return memProbe{gb: v / (1 << 30), source: "unified memory", ok: true}
			}
		}
	}
	// 3. No accelerator detected — bound by system RAM (CPU inference).
	if gb, ok := sysRAM(); ok {
		return memProbe{gb: gb, source: "system RAM (no GPU detected)", ok: true}
	}
	return memProbe{ok: false}
}

// systemRAMGB reads MemTotal from /proc/meminfo (Linux). Best-effort.
func systemRAMGB() (float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "MemTotal:") {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
				return kb / 1024 / 1024, true // kB → GB
			}
		}
	}
	return 0, false
}

// localLLMAdvice builds the doctor's hardware-aware model recommendation. It is
// always advisory (required:false, never a failure): warden recommends a
// local_llm_model from detected hardware, and the operator sets it — never an
// automatic silent swap.
func localLLMAdvice(cfg config.Config, mem memProbe) checkResult {
	if !mem.ok {
		return checkResult{name: "local llm", required: false, ok: true,
			detail: "hardware undetected → recommend qwen2.5-coder:1.5b (conservative floor); override by setting local_llm.model in your config file (`wd config path`)"}
	}
	rec := recommendModel(mem.gb)
	detail := fmt.Sprintf("detected ~%.0f GB %s → recommend %s (run `wd llm suggest` for memory-ranked options)", mem.gb, mem.source, rec)
	switch {
	case !cfg.GetLocalLLM():
		detail += "; enable the local model by setting local_llm: true in your config file (the orchestrator needs it; `wd config path`)"
	case cfg.LocalLLM.Model == rec:
		detail += "; configured model matches ✓"
	default:
		configured := cfg.LocalLLM.Model
		if strings.TrimSpace(configured) == "" {
			configured = "(unset)"
		}
		detail += fmt.Sprintf("; configured %s — change local_llm.model to %s in your config file (`wd config path`)", configured, rec)
	}
	return checkResult{name: "local llm", required: false, ok: true, detail: detail}
}
