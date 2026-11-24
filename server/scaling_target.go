package server

import (
	"sync/atomic"
	"time"
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
	IsScaling() bool
	ScaleDownPolicy() ScaleDownPolicy
	SetScaleDownPolicy(ScaleDownPolicy)
}

// safeScalingKey nil-safe function to return the scaling key of the given target, or an empty string if the target is nil.
func safeScalingKey(scalingTarget ScalingTarget) string {
	if scalingTarget == nil {
		return ""
	}
	return scalingTarget.ScalingKey()
}

type ScalingIndicator struct {
	scaling    *atomic.Bool
	idlePolicy atomic.Pointer[ScaleDownPolicy]
}

func (i *ScalingIndicator) StartScaling() bool {
	return i.scaling.CompareAndSwap(false, true)
}

func (i *ScalingIndicator) IsScaling() bool {
	return i.scaling.Load()
}

func (i *ScalingIndicator) EndScaling() bool {
	return i.scaling.CompareAndSwap(true, false)
}

// ScaleDownPolicy is resource configuration, independent of its hostnames.
// The zero value inherits the global delay. Pending means discovery has not yet
// resolved the scalable resource's metadata, so initial scheduling must wait.
type ScaleDownPolicy struct {
	delay   time.Duration
	pending bool
}

func (p ScaleDownPolicy) resolve(fallback time.Duration) time.Duration {
	if p.delay > 0 {
		return p.delay
	}
	return fallback
}

func (i *ScalingIndicator) ScaleDownPolicy() ScaleDownPolicy {
	if policy := i.idlePolicy.Load(); policy != nil {
		return *policy
	}
	return ScaleDownPolicy{}
}

// SetScaleDownPolicy publishes a value snapshot without changing an armed timer.
func (i *ScalingIndicator) SetScaleDownPolicy(policy ScaleDownPolicy) {
	if policy == (ScaleDownPolicy{}) {
		i.idlePolicy.Store(nil)
	} else {
		i.idlePolicy.Store(&policy)
	}
}
