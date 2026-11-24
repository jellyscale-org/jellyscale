package transcode

// Kernel is what a worker reads of the machine's CPU, to learn its capacity (JS-177). Worker.Kernel is nil for the
// kernel's own figures (osKernel); a test sets its own.
type Kernel interface {
	// Readable is nil when the scheduler's figures can be read. Asked once, at the worker's start.
	Readable() error
	// Allotment is the CPUs the worker may use: a container's cpu.max as written, which may be fractional, else the
	// processors the process may use, and never more than those.
	Allotment() float64
	// Demand is the CPU-seconds per second the processes wanted over about the last ten seconds: time on a CPU plus time
	// waiting on a run queue. The wait is what shows a CPU that others take or a cgroup throttles.
	Demand(pids []int) (float64, error)
}

// systemKernel is the kernel of a worker that is given none. It is shared, as the machine is.
var systemKernel Kernel = &osKernel{}

func (w *Worker) kernel() Kernel {
	if w.Kernel != nil {
		return w.Kernel
	}
	return systemKernel
}
