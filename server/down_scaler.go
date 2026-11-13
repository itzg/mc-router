package server

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// TODO need to re-evaluate the awkwardness of DownScaler in the overall whole flow.
// Maybe it needs to be renamed too, such as  DownScalingTimers

type IDownScaler interface {
	Reset()
	Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes, policy ScaleDownPolicy)
	Cancel(scalingTarget ScalingTarget)
	// ConnectionOpened/Closed track only connections configured to reset idle timers.
	ConnectionOpened(scalingTarget ScalingTarget)
	ConnectionClosed(scalingTarget ScalingTarget)
	HandleContextDone(ctx context.Context)
}

func NewDownScaler(enabled bool, delay time.Duration) IDownScaler {
	return &downScalerImpl{
		enabled: enabled,
		delay:   delay,
		timers:  make(map[string]*time.Timer),
		active:  make(map[string]int),
	}
}

type downScalerImpl struct {
	sync.Mutex
	enabled bool
	delay   time.Duration
	timers  map[string]*time.Timer
	// Unlike Connector's total target count, this excludes status requests unless
	// status timer resets are enabled. Guarded by the same lock as the timers.
	active map[string]int
}

func (ds *downScalerImpl) Reset() {
	ds.Lock()
	defer ds.Unlock()

	for _, t := range ds.timers {
		t.Stop()
	}
	ds.timers = make(map[string]*time.Timer)
	// Route reloads reset timers while connected players remain active.
}

func (ds *downScalerImpl) Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes, policy ScaleDownPolicy) {
	if !ds.enabled || scalingTarget == nil {
		return
	}
	// Policy is captured by the caller before taking the timer lock, avoiding
	// a route-lock/timer-lock inversion during registration and cancellation.
	delay := policy.resolve(ds.delay)
	ds.Lock()
	defer ds.Unlock()

	key := scalingTarget.ScalingKey()
	if ds.active[key] > 0 {
		return
	}
	if _, exists := ds.timers[key]; exists {
		// Already scheduled; prevent duplicate scale-down for same target
		return
	}

	logrus.WithField("scalingTarget", scalingTarget).
		WithField("delay", delay).
		Debug("Starting scale-down timer")

	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		sleeper := routes.GetSleeper(scalingTarget)
		logrus.WithField("scalingTarget", scalingTarget).
			WithField("found", sleeper != nil).
			Debug("Looking for sleeper to use")
		// Resolve the sleeper before taking the timer lock to preserve the route
		// -> timer lock order. Cancellation or a replacement while resolving it
		// must invalidate this callback, even if Timer.Stop returned false.
		ds.Lock()
		if ds.timers[key] != timer {
			ds.Unlock()
			return
		}
		delete(ds.timers, key)
		canScale := sleeper != nil && ds.active[key] == 0 && ctx.Err() == nil && scalingTarget.StartScaling()
		ds.Unlock()
		if canScale {
			defer scalingTarget.EndScaling()

			logrus.WithField("scalingTarget", scalingTarget).Info("Scaling down backend server")
			if err := sleeper(ctx); err != nil {
				logrus.WithError(err).
					WithField("scalingTarget", scalingTarget).
					Error("Error while executing sleeper function")
			}
		}
	})
	ds.timers[key] = timer
}

func (ds *downScalerImpl) Cancel(scalingTarget ScalingTarget) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled || scalingTarget == nil {
		return
	}

	ds.cancelLocked(scalingTarget)
}

// cancelLocked is shared by explicit cancellation and connection admission.
func (ds *downScalerImpl) cancelLocked(scalingTarget ScalingTarget) {
	key := scalingTarget.ScalingKey()
	if t, ok := ds.timers[key]; ok {
		logrus.WithField("scalingTarget", scalingTarget).Debug("Canceling scale-down timer")
		t.Stop()
		delete(ds.timers, key)
	}
}

func (ds *downScalerImpl) ConnectionOpened(target ScalingTarget) {
	if !ds.enabled || target == nil {
		return
	}
	ds.Lock()
	defer ds.Unlock()
	ds.active[target.ScalingKey()]++
	ds.cancelLocked(target)
}

func (ds *downScalerImpl) ConnectionClosed(target ScalingTarget) {
	if !ds.enabled || target == nil {
		return
	}
	ds.Lock()
	defer ds.Unlock()
	key := target.ScalingKey()
	if ds.active[key] <= 1 {
		delete(ds.active, key)
	} else {
		ds.active[key]--
	}
}

func (ds *downScalerImpl) HandleContextDone(ctx context.Context) {
	go func() {
		<-ctx.Done()
		ds.Reset()
	}()
}
