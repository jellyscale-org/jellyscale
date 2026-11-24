package transcode

import (
	"sync"
	"time"
)

// JS-177 (D-064): what a worker reads of the CPU, set by the test. It is in an untagged file so that the unit tier can
// inject the CPUs a worker may use instead of reading the host's cpu.max, which on a runner with a cgroup limit is not
// GOMAXPROCS and would change the answer of a test that forces GOMAXPROCS. It implements the Kernel interface the build
// adds (capacity_learn_integration_test.go lists it).
type fakeKernel struct {
	mu     sync.Mutex
	cpus   float64
	demand float64
	err    error
	asked  []time.Time // When each Demand was asked: the worker's sampler, by mechanism.
}

func (k *fakeKernel) Readable() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

func (k *fakeKernel) Allotment() float64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cpus
}

func (k *fakeKernel) Demand([]int) (float64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, time.Now())
	return k.demand, k.err
}

// demandTimes are the moments Demand was asked, oldest first; its length is the count.
func (k *fakeKernel) demandTimes() []time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]time.Time(nil), k.asked...)
}

func (k *fakeKernel) wants(demand float64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.demand = demand
}

// starvedKernel is a kernel on which the worker's ffmpeg wanted 1.5 times its 4 CPUs: a throttled worker, or one starved by
// hogs. idleKernel is one on which it wanted a sixteenth of them: the CPU is not what slows its jobs.
func starvedKernel() *fakeKernel { return &fakeKernel{cpus: 4, demand: 6} }
func idleKernel() *fakeKernel    { return &fakeKernel{cpus: 4, demand: 0.25} }
