package server

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type IDownScaler interface {
	Reset()
	Begin(serverAddress string)
	Cancel(serverAddress string)
}

var DownScaler IDownScaler

func NewDownScaler(ctx context.Context, enabled bool, delay time.Duration) IDownScaler {
	ds := &downScalerImpl{
		enabled: enabled,
		delay: delay,
		parentContext: ctx,
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
	for _, scaleDownCancel := range ds.contextCancellations {
		scaleDownCancel()
	}
	ds.contextCancellations = make(map[string]context.CancelFunc)
}

func (ds *downScalerImpl) Begin(serverAddress string) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	// If an existing scale down routine exists, cancel it
	if scaleDownCancel, ok := ds.contextCancellations[serverAddress]; ok {
		scaleDownCancel()
	}
	
	logrus.WithField("serverAddress", serverAddress).Debug("Beginning scale down")
	scaleDownContext, scaleDownContextCancellation := context.WithCancel(ds.parentContext)
	ds.contextCancellations[serverAddress] = scaleDownContextCancellation
	go ds.scaleDown(scaleDownContext, serverAddress)
}

func (ds *downScalerImpl) Cancel(serverAddress string) {
	ds.Lock()
	defer ds.Unlock()

	if !ds.enabled {
		return
	}

	if scaleDownContextCancellation, ok := ds.contextCancellations[serverAddress]; ok {
		logrus.WithField("serverAddress", serverAddress).Debug("Canceling scale down")
		scaleDownContextCancellation()
		delete(ds.contextCancellations, serverAddress)
	}
}

func (ds *downScalerImpl) scaleDown(ctx context.Context, serverAddress string) {
	for {
		select {
			case <-ctx.Done():
				return
			case <-time.After(ds.delay):
				_, _, _, sleeper := Routes.FindBackendForServerAddress(ctx, serverAddress)
				if sleeper == nil {
					return
				}
				if err := sleeper(ctx); err != nil {
					logrus.WithField("serverAddress", serverAddress).WithError(err).Error("failed to scale down backend")
				}
				return
		}
	}
}
