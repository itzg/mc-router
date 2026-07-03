package server

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// TODO need to re-evalute the awkwardness of DownScaler in the overall whole flow.
// For example, I'm not sure where waker/sleeper fits and it's part of a Routes<->DownScaler cycle.
// It also has an "enabled" flag, but why not just have this whole thing be optional/nil-able as the caller end.

type IDownScaler interface {
	Reset()
	Start(ctx context.Context, backendEndpoint string, routes IRoutes)
	Cancel(backendEndpoint string)
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

func (ds *downScalerImpl) Start(ctx context.Context, backendEndpoint string, routes IRoutes) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	// If an existing scale down routine exists, cancel it
	if scaleDownCancel, ok := ds.contextCancellations[backendEndpoint]; ok {
		scaleDownCancel()
	}

	scaleDownContext, scaleDownContextCancellation := context.WithCancel(ctx)
	ds.contextCancellations[backendEndpoint] = scaleDownContextCancellation
	go ds.scaleDown(scaleDownContext, backendEndpoint, routes)
}

func (ds *downScalerImpl) Cancel(backendEndpoint string) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	if scaleDownContextCancellation, ok := ds.contextCancellations[backendEndpoint]; ok {
		logrus.WithField("backendEndpoint", backendEndpoint).Debug("Canceling scale down")
		scaleDownContextCancellation()
		delete(ds.contextCancellations, backendEndpoint)
	}
}

func (ds *downScalerImpl) scaleDown(ctx context.Context, backendEndpoint string, routes IRoutes) {
	logrus.WithField("backendEndpoint", backendEndpoint).
		WithField("delay", ds.delay).
		Debug("Starting scale-down timer")
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ds.delay):
			sleepers := routes.GetSleepers(backendEndpoint)
			logrus.WithField("backendEndpoint", backendEndpoint).
				WithField("sleepers", len(sleepers)).
				Debug("Found sleepers to use")
			if len(sleepers) == 0 {
				return
			}
			for _, sleeper := range sleepers {
				go func(s SleeperFunc) {
					err := s(ctx)
					if err != nil {
						logrus.WithError(err).
							WithField("backendEndpoint", backendEndpoint).
							Error("Error while executing sleeper function")
					}
				}(sleeper)
			}
			return
		}
	}
}
