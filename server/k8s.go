package server

import (
	"net"
	"strconv"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	AnnotationExternalServerName = "mc-router.itzg.me/externalServerName"
	AnnotationDefaultServer      = "mc-router.itzg.me/defaultServer"
)

type IK8sWatcher interface {
	StartWithConfig(kubeConfigFile string) error
	StartInCluster() error
	Stop()
}

var K8sWatcher IK8sWatcher = &k8sWatcherImpl{}

type k8sWatcherImpl struct {
	stop chan struct{}
}

func (w *k8sWatcherImpl) StartInCluster() error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return errors.Wrap(err, "Unable to load in-cluster config")
	}

	return w.startWithLoadedConfig(config)
}

func (w *k8sWatcherImpl) StartWithConfig(kubeConfigFile string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigFile)
	if err != nil {
		return errors.Wrap(err, "Could not load kube config file")
	}

	return w.startWithLoadedConfig(config)
}

func (w *k8sWatcherImpl) startWithLoadedConfig(config *rest.Config) error {
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

					if routableService.externalServiceName != "" {
						Routes.CreateMapping(routableService.externalServiceName, routableService.containerEndpoint)
					} else {
						Routes.SetDefaultRoute(routableService.containerEndpoint)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				routableService := extractRoutableService(obj)
				if routableService != nil {
					logrus.WithField("routableService", routableService).Debug("DELETE")

					if routableService.externalServiceName != "" {
						Routes.DeleteMapping(routableService.externalServiceName)
					} else {
						Routes.SetDefaultRoute("")
					}
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

					if oldRoutableService.externalServiceName != "" && newRoutableService.externalServiceName != "" {
						Routes.DeleteMapping(oldRoutableService.externalServiceName)
						Routes.CreateMapping(newRoutableService.externalServiceName, newRoutableService.containerEndpoint)
					} else {
						Routes.SetDefaultRoute(newRoutableService.containerEndpoint)
					}
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

	if externalServiceName, exists := service.Annotations[AnnotationExternalServerName]; exists {
		return buildDetails(service, externalServiceName)
	} else if _, exists := service.Annotations[AnnotationDefaultServer]; exists {
		return buildDetails(service, "")
	}

	return nil
}

func buildDetails(service *v1.Service, externalServiceName string) *routableService {
	clusterIp := service.Spec.ClusterIP
	port := "25565"
	for _, p := range service.Spec.Ports {
		if p.Name == "mc-router" {
			port = strconv.Itoa(int(p.Port))
		}
	}
	rs := &routableService{
		externalServiceName: externalServiceName,
		containerEndpoint:   net.JoinHostPort(clusterIp, port),
	}
	return rs
}
