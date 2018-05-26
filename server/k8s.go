package server

import (
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"net"
)

type IK8sWatcher interface {
	Start(kubeConfigFile string) error
	Stop()
}

var K8sWatcher IK8sWatcher = &k8sWatcherImpl{}

type k8sWatcherImpl struct {
	stop chan struct{}
}

func (w *k8sWatcherImpl) Start(kubeConfigFile string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigFile)
	if err != nil {
		return errors.Wrap(err, "Could not load kube config file")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "Could not create kube clientset")
	}

	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		string(v1.ResourceServices),
		v1.NamespaceAll,
		fields.Everything(),
	)

	_, controller := cache.NewInformer(
		watchlist,
		&v1.Service{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				routableService := extractRoutableService(obj)
				if routableService != nil {
					logrus.WithField("routableService", routableService).Debug("ADD")

					Routes.CreateMapping(routableService.externalServiceName, routableService.containerEndpoint)
				}
			},
			DeleteFunc: func(obj interface{}) {
				routableService := extractRoutableService(obj)
				if routableService != nil {
					logrus.WithField("routableService", routableService).Debug("DELETE")

					Routes.DeleteMapping(routableService.externalServiceName)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldRoutableService := extractRoutableService(oldObj)
				newRoutableService := extractRoutableService(newObj)
				if oldRoutableService != nil && newRoutableService != nil {
					logrus.WithFields(logrus.Fields{
						"old": oldRoutableService,
						"new": newRoutableService,
					}).Debug("UPDATE")

					Routes.DeleteMapping(oldRoutableService.externalServiceName)
					Routes.CreateMapping(newRoutableService.externalServiceName, newRoutableService.containerEndpoint)
				}
			},
		},
	)

	w.stop = make(chan struct{}, 1)
	logrus.Info("Monitoring kubernetes for minecraft services")
	go controller.Run(w.stop)

	return nil
}

func (w *k8sWatcherImpl) Stop() {
	if w.stop != nil {
		w.stop <- struct{}{}
	}
}

type routableService struct {
	externalServiceName string
	containerEndpoint   string
}

func extractRoutableService(obj interface{}) *routableService {
	service, ok := obj.(*v1.Service)
	if !ok {
		return nil
	}

	if externalServiceName, exists := service.Annotations["mc-router.itzg.me/externalServerName"]; exists {
		clusterIp := service.Spec.ClusterIP
		port := "25565"
		for _, p := range service.Spec.Ports {
			if p.Port == 25565 {
				if p.TargetPort.String() != "" {
					port = p.TargetPort.String()
				}
			}
		}
		rs := &routableService{
			externalServiceName: externalServiceName,
			containerEndpoint:   net.JoinHostPort(clusterIp, port),
		}
		return rs
	}

	return nil
}
