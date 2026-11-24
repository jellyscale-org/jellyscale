package transcode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// osKernel reads cgroup v2's cpu.max and each thread's scheduler accounting (/proc/<pid>/task/<tid>/schedstat: time on a
// CPU and time on a run queue, in nanoseconds). The run-queue time already includes the time a cgroup throttles a thread
// (docs/transcoding.md, the JS-177 measurement), so cpu.stat's throttled time is not added to it.
type osKernel struct {
	mu      sync.Mutex
	history map[string][]schedReading
}

const cgroupRoot = "/sys/fs/cgroup"

// schedWindow is how far back a thread's demand is measured: the window the job speeds are read over.
const schedWindow = 10 * time.Second

type schedReading struct {
	at    time.Time
	nanos int64
}

func (k *osKernel) Readable() error {
	_, err := schedNanos("/proc/self/task/" + strconv.Itoa(os.Getpid()) + "/schedstat")
	return err
}

func (k *osKernel) Allotment() float64 {
	processors := float64(runtime.GOMAXPROCS(0))
	if limit, ok := cgroupLimit(); ok {
		return min(limit, processors)
	}
	return processors
}

// cgroupLimit is the smallest cpu.max in the process's cgroup and its ancestors, in CPUs.
func cgroupLimit() (limit float64, found bool) {
	own, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false
	}
	_, path, ok := strings.Cut(strings.TrimSpace(string(own)), "::")
	if !ok {
		return 0, false
	}
	for dir := filepath.Join(cgroupRoot, path); strings.HasPrefix(dir, cgroupRoot); dir = filepath.Dir(dir) {
		if cpus, ok := readCPUMax(filepath.Join(dir, "cpu.max")); ok && (!found || cpus < limit) {
			limit, found = cpus, true
		}
		if dir == cgroupRoot {
			break
		}
	}
	return limit, found
}

// readCPUMax reads "<quota> <period>", or "max <period>" for no limit.
func readCPUMax(file string) (float64, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	quota, period, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	if !ok || quota == "max" {
		return 0, false
	}
	quotaMicros, quotaErr := strconv.ParseFloat(quota, 64)
	periodMicros, periodErr := strconv.ParseFloat(period, 64)
	if quotaErr != nil || periodErr != nil || periodMicros <= 0 {
		return 0, false
	}
	return quotaMicros / periodMicros, true
}

// schedNanos is the time on a CPU plus the time on a run queue that a schedstat file reports.
func schedNanos(file string) (int64, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, fmt.Errorf("%s: no scheduler accounting", file)
	}
	running, runErr := strconv.ParseInt(fields[0], 10, 64)
	waiting, waitErr := strconv.ParseInt(fields[1], 10, 64)
	return running + waiting, errors.Join(runErr, waitErr)
}

func (k *osKernel) Demand(pids []int) (float64, error) {
	now := time.Now()
	k.mu.Lock()
	defer k.mu.Unlock()
	seen := map[string][]schedReading{}
	var readErr error
	for _, pid := range pids {
		tasks, err := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/schedstat")
		if err != nil {
			readErr = err
		}
		for _, file := range tasks {
			nanos, err := schedNanos(file)
			if err != nil {
				continue // The thread ended between the listing and the read.
			}
			seen[file] = trimReadings(append(k.history[file], schedReading{now, nanos}), now)
		}
	}
	k.history = seen
	demand := 0.0
	for _, readings := range seen {
		first, last := readings[0], readings[len(readings)-1]
		if span := last.at.Sub(first.at).Seconds(); span > 0 {
			demand += float64(last.nanos-first.nanos) / 1e9 / span
		}
	}
	return demand, readErr
}

// trimReadings keeps the newest reading that is at least a window old, and every one after it.
func trimReadings(readings []schedReading, now time.Time) []schedReading {
	for len(readings) > 1 && now.Sub(readings[1].at) >= schedWindow {
		readings = readings[1:]
	}
	return readings
}
