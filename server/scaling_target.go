package server

import (
	"sync/atomic"
)

type ScalingTarget interface {
	// StartScaling and EndScaling are called when the target scales up (via WakerFunc) and scales down (via SleeperFunc).
	// Returns true if this is the first state change, which is intended to be used in a guarded conditional such as
	//
	// 	if scalingTarget.StartScaling() {
	//    defer scalingTarget.EndScaling()
	//    // invoke sleeper/waker
	// 	}
	StartScaling() bool
	EndScaling() bool
	// ScalingKey returns a unique identifier for the target suitable for use as a map key.
	ScalingKey() string
}

type ScalingIndicator struct {
	scaling *atomic.Bool
}

func (i *ScalingIndicator) StartScaling() bool {
	return i.scaling.CompareAndSwap(false, true)
}

func (i *ScalingIndicator) EndScaling() bool {
	return i.scaling.CompareAndSwap(true, false)
}
