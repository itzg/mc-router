package server

import (
	"fmt"
	"sync/atomic"
)

type ScalingTarget interface {
	fmt.Stringer
	// IsScaling returns true if the target is currently scaling up or down
	IsScaling() bool
	// StartScaling and EndScaling are called when the target starts and stops scaling. Returns true if this is the first state change.
	StartScaling() bool
	EndScaling() bool
	Equal(other ScalingTarget) bool
	// Key returns a unique identifier for the target or "" if the instance is nil
	Key() string
}

type ScalingIndicator struct {
	scaling *atomic.Bool
}

func (i *ScalingIndicator) IsScaling() bool {
	return i.scaling.Load()
}

func (i *ScalingIndicator) StartScaling() bool {
	return i.scaling.CompareAndSwap(false, true)
}

func (i *ScalingIndicator) EndScaling() bool {
	return i.scaling.CompareAndSwap(true, false)
}

type namedScalingTarget struct {
	ScalingIndicator
	name string
}

func NamedScalingTarget(name string) ScalingTarget {
	return &namedScalingTarget{name: name, ScalingIndicator: ScalingIndicator{scaling: &atomic.Bool{}}}
}

func (n *namedScalingTarget) String() string {
	if n == nil {
		return ""
	}
	return n.name
}

func (n *namedScalingTarget) Key() string {
	if n == nil {
		return ""
	}
	return n.name
}

func (n *namedScalingTarget) Equal(other ScalingTarget) bool {
	if n == nil || other == nil {
		return n == nil && other == nil
	}
	if otherNamed, ok := other.(*namedScalingTarget); ok {
		return n.name == otherNamed.name
	}
	return false
}
