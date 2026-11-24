//go:build !linux

package transcode

import (
	"errors"
	"runtime"
)

// osKernel has no scheduler accounting here: a worker learns nothing and says so (D-066).
type osKernel struct{}

var errNoScheduler = errors.New("this system has no scheduler accounting the worker can read")

func (k *osKernel) Readable() error { return errNoScheduler }

func (k *osKernel) Allotment() float64 { return float64(runtime.GOMAXPROCS(0)) }

func (k *osKernel) Demand([]int) (float64, error) { return 0, errNoScheduler }
