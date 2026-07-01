// Command telemetry_debug is a debug build of the telemetry agent.
//
// It behaves exactly like the production telemetry agent (same /proc sampling,
// same 90/80 hysteresis, same TELEMETRY_PORT/2136 config) but its /health
// response also exposes the raw CPU and memory percentages behind the verdict:
//
//	{"overloaded": false, "cpu": 12.3, "mem": 34.5}
//
// Use it to watch the live numbers while tuning. The production binary keeps the
// strict {"overloaded": bool} contract.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultPort = "2136"
	portEnv     = "TELEMETRY_PORT"

	// Hysteresis band: trip at the high mark, only recover below the low mark.
	highThreshold = 0.90
	lowThreshold  = 0.80

	sampleInterval = 500 * time.Millisecond
)

// snapshot is the latest sampled state. The debug endpoint reports all of it.
type snapshot struct {
	Overloaded bool    `json:"overloaded"`
	CPU        float64 `json:"cpu"` // percent, 0-100
	Mem        float64 `json:"mem"` // percent, 0-100
}

// state holds the latest snapshot. It is written only by the sampler goroutine
// and read by HTTP handlers, so an atomic pointer is sufficient.
var state atomic.Pointer[snapshot]

func main() {
	port := os.Getenv(portEnv)
	if port == "" {
		port = defaultPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatalf("invalid %s=%q: must be a port number", portEnv, port)
	}

	state.Store(&snapshot{})

	go sampler()

	http.HandleFunc("/health", healthHandler)

	addr := ":" + port
	log.Printf("telemetry_debug listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state.Load())
}

// sampler periodically measures utilization and updates the snapshot. CPU
// utilization is a delta between consecutive /proc/stat reads, so it keeps the
// previous sample across ticks. Unlike the production agent, it also logs each
// tick so you can watch the live numbers.
func sampler() {
	prev, err := readCPU()
	if err != nil {
		log.Fatalf("reading /proc/stat: %v", err)
	}

	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for range ticker.C {
		cur, err := readCPU()
		if err != nil {
			log.Printf("reading /proc/stat: %v", err)
			continue
		}
		cpu := cpuUtil(prev, cur)
		prev = cur

		mem, err := readMemUsed()
		if err != nil {
			log.Printf("reading /proc/meminfo: %v", err)
			continue
		}

		overloaded := evaluate(state.Load().Overloaded, cpu, mem)
		state.Store(&snapshot{
			Overloaded: overloaded,
			CPU:        cpu * 100,
			Mem:        mem * 100,
		})
		log.Printf("cpu=%.1f%% mem=%.1f%% overloaded=%t", cpu*100, mem*100, overloaded)
	}
}

// evaluate applies the hysteresis rule. Once overloaded, both metrics must fall
// below the low threshold before recovering; otherwise either metric crossing
// the high threshold trips it.
func evaluate(currentlyOverloaded bool, cpu, mem float64) bool {
	if currentlyOverloaded {
		return !(cpu < lowThreshold && mem < lowThreshold)
	}
	return cpu > highThreshold || mem > highThreshold
}

type cpuTimes struct{ idle, total uint64 }

// readCPU parses the aggregate "cpu" line of /proc/stat. idle counts both the
// idle and iowait fields.
func readCPU() (cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, fmt.Errorf("unexpected /proc/stat format: %q", line)
	}

	var t cpuTimes
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuTimes{}, err
		}
		t.total += v
		if i == 3 || i == 4 { // idle, iowait
			t.idle += v
		}
	}
	return t, nil
}

// cpuUtil returns the busy fraction between two samples.
func cpuUtil(prev, cur cpuTimes) float64 {
	dTotal := cur.total - prev.total
	if dTotal == 0 {
		return 0
	}
	dIdle := cur.idle - prev.idle
	return float64(dTotal-dIdle) / float64(dTotal)
}

// readMemUsed returns used memory as a fraction of total, using MemAvailable as
// the kernel's estimate of what can be allocated without swapping.
func readMemUsed() (float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseMeminfoLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			total, haveTotal = val, true
		case "MemAvailable":
			avail, haveAvail = val, true
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if !haveTotal || total == 0 {
		return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	if !haveAvail {
		return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
	}
	return float64(total-avail) / float64(total), nil
}

// parseMeminfoLine splits a line like "MemTotal:  16384000 kB" into its key and
// numeric value (in kB).
func parseMeminfoLine(line string) (key string, val uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	key = strings.TrimSuffix(fields[0], ":")
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, v, true
}
