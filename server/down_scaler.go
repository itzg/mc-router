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
	Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes)
	Cancel(scalingTarget ScalingTarget)
	HandleContextDone(ctx context.Context)
}

func NewDownScaler(enabled bool, delay time.Duration) IDownScaler {
	return &downScalerImpl{
		enabled: enabled,
		delay:   delay,
		timers:  make(map[string]*time.Timer),
	}
}

type downScalerImpl struct {
	sync.Mutex
	enabled bool
	delay   time.Duration
	timers  map[string]*time.Timer
}

func (ds *downScalerImpl) Reset() {
	ds.Lock()
	defer ds.Unlock()

	for _, t := range ds.timers {
		t.Stop()
	}
	ds.timers = make(map[string]*time.Timer)
}

func (ds *downScalerImpl) Start(ctx context.Context, scalingTarget ScalingTarget, routes IRoutes) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	key := scalingTarget.ScalingKey()
	if _, exists := ds.timers[key]; exists {
		// Already scheduled; prevent duplicate scale-down for same target
		return
	}

	logrus.WithField("scalingTarget", scalingTarget).
		WithField("delay", ds.delay).
		Debug("Starting scale-down timer")

	ds.timers[key] = time.AfterFunc(ds.delay, func() {
		ds.Lock()
		delete(ds.timers, key)
		ds.Unlock()

		select {
		case <-ctx.Done():
			return
		default:
		}

		sleeper := routes.GetSleeper(scalingTarget)
		logrus.WithField("scalingTarget", scalingTarget).
			WithField("found", sleeper != nil).
			Debug("Looking for sleeper to use")
		if sleeper == nil {
			return
		}

		if scalingTarget.StartScaling() {
			defer scalingTarget.EndScaling()

			logrus.WithField("scalingTarget", scalingTarget).Info("Scaling-down")
			if err := sleeper(ctx); err != nil {
				logrus.WithError(err).
					WithField("scalingTarget", scalingTarget).
					Error("Error while executing sleeper function")
			}
		}
	})
}

func (ds *downScalerImpl) Cancel(scalingTarget ScalingTarget) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	key := scalingTarget.ScalingKey()
	if t, ok := ds.timers[key]; ok {
		logrus.WithField("scalingTarget", scalingTarget).Debug("Canceling scale-down timer")
		t.Stop()
		delete(ds.timers, key)
	}
}

func (ds *downScalerImpl) HandleContextDone(ctx context.Context) {
	go func() {
		<-ctx.Done()
		ds.Reset()
	}()
}
