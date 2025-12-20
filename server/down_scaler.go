package server

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type IDownScaler interface {
	Reset()
	Begin(backendEndpoint string)
	Cancel(backendEndpoint string)
}

var DownScaler IDownScaler

func NewDownScaler(ctx context.Context, enabled bool, delay time.Duration) IDownScaler {
	ds := &downScalerImpl{
		enabled:              enabled,
		delay:                delay,
		parentContext:        ctx,
		contextCancellations: make(map[string]context.CancelFunc),
	}

	return ds
}

type downScalerImpl struct {
	sync.RWMutex
	enabled              bool
	delay                time.Duration
	parentContext        context.Context
	contextCancellations map[string]context.CancelFunc
}

func (ds *downScalerImpl) Reset() {
	// Cancel all existing scale down routines
	for _, scaleDownCancel := range ds.contextCancellations {
		scaleDownCancel()
	}
	ds.contextCancellations = make(map[string]context.CancelFunc)
}

func (ds *downScalerImpl) Begin(backendEndpoint string) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	// If an existing scale down routine exists, cancel it
	if scaleDownCancel, ok := ds.contextCancellations[backendEndpoint]; ok {
		scaleDownCancel()
	}

	logrus.WithField("backendEndpoint", backendEndpoint).Debug("Beginning scale down")
	scaleDownContext, scaleDownContextCancellation := context.WithCancel(ds.parentContext)
	ds.contextCancellations[backendEndpoint] = scaleDownContextCancellation
	go ds.scaleDown(scaleDownContext, backendEndpoint)
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

func (ds *downScalerImpl) scaleDown(ctx context.Context, backendEndpoint string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ds.delay):
			sleepers := Routes.GetSleepers(backendEndpoint)
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
