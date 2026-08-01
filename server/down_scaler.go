package server

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// TODO need to re-evaluate the awkwardness of DownScaler in the overall whole flow.
// For example, I'm not sure where waker/sleeper fits and it's part of a Routes<->DownScaler cycle.
// It also has an "enabled" flag, but why not just have this whole thing be optional/nil-able as the caller end.
// Maybe it needs to be renamed too, such as ScalingTracker or ScalingTimers

type IDownScaler interface {
	Reset()
	Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes)
	Cancel(scalingTarget ScalingTarget)
}

func NewDownScaler(enabled bool, delay time.Duration) IDownScaler {
	ds := &downScalerImpl{
		enabled:              enabled,
		delay:                delay,
		contextCancellations: make(map[string]context.CancelFunc),
	}

	return ds
}

type downScalerImpl struct {
	sync.RWMutex
	enabled              bool
	delay                time.Duration
	contextCancellations map[string]context.CancelFunc
}

func (ds *downScalerImpl) Reset() {
	// Cancel all existing scale down routines
	for _, scaleDownCancel := range ds.contextCancellations {
		scaleDownCancel()
	}
	ds.contextCancellations = make(map[string]context.CancelFunc)
}

func (ds *downScalerImpl) Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	// If an existing scale down routine exists, cancel it
	if scaleDownCancel, ok := ds.contextCancellations[scalingTarget.ScalingKey()]; ok {
		scaleDownCancel()
	}

	scaleDownContext, scaleDownContextCancellation := context.WithCancel(ctx)
	ds.contextCancellations[scalingTarget.ScalingKey()] = scaleDownContextCancellation
	go ds.scaleDown(scaleDownContext, scalingTarget, routes)
}

func (ds *downScalerImpl) Cancel(scalingTarget ScalingTarget) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	if scaleDownContextCancellation, ok := ds.contextCancellations[scalingTarget.ScalingKey()]; ok {
		logrus.WithField("scalingTarget", scalingTarget).Debug("Canceling scale down")
		scaleDownContextCancellation()
		delete(ds.contextCancellations, scalingTarget.ScalingKey())
	}
}

func (ds *downScalerImpl) scaleDown(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes) {
	logrus.WithField("scalingTarget", scalingTarget).
		WithField("delay", ds.delay).
		Debug("Starting scale-down timer")
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ds.delay):
			sleeper := routes.GetSleeper(scalingTarget)
			logrus.WithField("scalingTarget", scalingTarget).
				WithField("sleeper", sleeper != nil).
				Debug("Found sleeper to use")
			if sleeper == nil {
				return
			}
			go func() {
				if scalingTarget.StartScaling() {
					defer scalingTarget.EndScaling()

					err := sleeper(ctx)
					if err != nil {
						logrus.WithError(err).
							WithField("scalingTarget", scalingTarget).
							Error("Error while executing sleeper function")
					}
				}
			}()
			return
		}
	}
}
