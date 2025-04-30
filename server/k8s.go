package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	autoscaling "k8s.io/api/autoscaling/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	StartWithConfig(kubeConfigFile string, autoScaleUp bool) error
	StartInCluster(autoScaleUp bool) error
	Stop()
}

var K8sWatcher IK8sWatcher = &k8sWatcherImpl{}

type k8sWatcherImpl struct {
	sync.RWMutex
	// The key in mappings is a Service, and the value the StatefulSet name
	mappings map[string]string

	clientset *kubernetes.Clientset
	stop      chan struct{}
}

func (w *k8sWatcherImpl) StartInCluster(autoScaleUp bool) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return errors.Wrap(err, "Unable to load in-cluster config")
	}

	return w.startWithLoadedConfig(config, autoScaleUp)
}

func (w *k8sWatcherImpl) StartWithConfig(kubeConfigFile string, autoScaleUp bool) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigFile)
	if err != nil {
		return errors.Wrap(err, "Could not load kube config file")
	}

	return w.startWithLoadedConfig(config, autoScaleUp)
}

func (w *k8sWatcherImpl) startWithLoadedConfig(config *rest.Config, autoScaleUp bool) error {
	w.stop = make(chan struct{}, 1)

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "Could not create kube clientset")
	}
	w.clientset = clientset

	_, serviceController := cache.NewInformer(
		cache.NewListWatchFromClient(
			clientset.CoreV1().RESTClient(),
			string(core.ResourceServices),
			core.NamespaceAll,
			fields.Everything(),
		),
		&core.Service{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    w.handleAdd,
			DeleteFunc: w.handleDelete,
			UpdateFunc: w.handleUpdate,
		},
	)
	go serviceController.Run(w.stop)

	w.mappings = make(map[string]string)
	if autoScaleUp {
		_, statefulSetController := cache.NewInformer(
			cache.NewListWatchFromClient(
				clientset.AppsV1().RESTClient(),
				"statefulSets",
				core.NamespaceAll,
				fields.Everything(),
			),
			&apps.StatefulSet{},
			0,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					statefulSet, ok := obj.(*apps.StatefulSet)
					if !ok {
						return
					}
					w.RLock()
					defer w.RUnlock()
					w.mappings[statefulSet.Spec.ServiceName] = statefulSet.Name
				},
				DeleteFunc: func(obj interface{}) {
					statefulSet, ok := obj.(*apps.StatefulSet)
					if !ok {
						return
					}
					w.RLock()
					defer w.RUnlock()
					delete(w.mappings, statefulSet.Spec.ServiceName)
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					oldStatefulSet, ok := oldObj.(*apps.StatefulSet)
					if !ok {
						return
					}
					newStatefulSet, ok := newObj.(*apps.StatefulSet)
					if !ok {
						return
					}
					w.RLock()
					defer w.RUnlock()
					delete(w.mappings, oldStatefulSet.Spec.ServiceName)
					w.mappings[newStatefulSet.Spec.ServiceName] = newStatefulSet.Name
				},
			},
		)
		go statefulSetController.Run(w.stop)
	}

	logrus.Info("Monitoring Kubernetes for Minecraft services")
	return nil
}

// oldObj and newObj are expected to be *v1.Service
func (w *k8sWatcherImpl) handleUpdate(oldObj interface{}, newObj interface{}) {
	for _, oldRoutableService := range w.extractRoutableServices(oldObj) {
		logrus.WithFields(logrus.Fields{
			"old": oldRoutableService,
		}).Debug("UPDATE")
		if oldRoutableService.externalServiceName != "" {
			Routes.DeleteMapping(oldRoutableService.externalServiceName)
		}
	}

	for _, newRoutableService := range w.extractRoutableServices(newObj) {
		logrus.WithFields(logrus.Fields{
			"new": newRoutableService,
		}).Debug("UPDATE")
		if newRoutableService.externalServiceName != "" {
			Routes.CreateMapping(newRoutableService.externalServiceName, newRoutableService.containerEndpoint, newRoutableService.autoScaleUp, newRoutableService.autoScaleDown)
		} else {
			Routes.SetDefaultRoute(newRoutableService.containerEndpoint)
		}
	}
}

// obj is expected to be a *v1.Service
func (w *k8sWatcherImpl) handleDelete(obj interface{}) {
	routableServices := w.extractRoutableServices(obj)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("DELETE")

			if routableService.externalServiceName != "" {
				Routes.DeleteMapping(routableService.externalServiceName)
			} else {
				Routes.SetDefaultRoute("")
			}
		}
	}
}

// obj is expected to be a *v1.Service
func (w *k8sWatcherImpl) handleAdd(obj interface{}) {
	routableServices := w.extractRoutableServices(obj)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("ADD")

			if routableService.externalServiceName != "" {
				Routes.CreateMapping(routableService.externalServiceName, routableService.containerEndpoint, routableService.autoScaleUp, routableService.autoScaleDown)
			} else {
				Routes.SetDefaultRoute(routableService.containerEndpoint)
			}
		}
	}
}

func (w *k8sWatcherImpl) Stop() {
	if w.stop != nil {
		close(w.stop)
	}
}

type routableService struct {
	externalServiceName string
	containerEndpoint   string
	autoScaleUp         ScalerFunc
	autoScaleDown       ScalerFunc
}

// obj is expected to be a *v1.Service
func (w *k8sWatcherImpl) extractRoutableServices(obj interface{}) []*routableService {
	service, ok := obj.(*core.Service)
	if !ok {
		return nil
	}

	routableServices := make([]*routableService, 0)
	if externalServiceName, exists := service.Annotations[AnnotationExternalServerName]; exists {
		serviceNames := strings.Split(externalServiceName, ",")
		for _, serviceName := range serviceNames {
			routableServices = append(routableServices, w.buildDetails(service, serviceName))
		}
		return routableServices
	} else if _, exists := service.Annotations[AnnotationDefaultServer]; exists {
		return []*routableService{w.buildDetails(service, "")}
	}

	return nil
}

func (w *k8sWatcherImpl) buildDetails(service *core.Service, externalServiceName string) *routableService {
	clusterIp := service.Spec.ClusterIP
	port := "25565"
	for _, p := range service.Spec.Ports {
		if p.Name == "mc-router" || p.Name == "minecraft" {
			port = strconv.Itoa(int(p.Port))
		}
	}
	rs := &routableService{
		externalServiceName: externalServiceName,
		containerEndpoint:   net.JoinHostPort(clusterIp, port),
		autoScaleUp:         w.buildScaleFunction(service, 0, 1),
		autoScaleDown:       w.buildScaleFunction(service, 1, 0),
	}
	return rs
}

func (w *k8sWatcherImpl) buildScaleFunction(service *core.Service, from int, to int) ScalerFunc {
	return func(ctx context.Context) error {
		serviceName := service.Name
		if statefulSetName, exists := w.mappings[serviceName]; exists {
			if scale, err := w.clientset.AppsV1().StatefulSets(service.Namespace).GetScale(ctx, statefulSetName, meta.GetOptions{}); err == nil {
				replicas := scale.Status.Replicas
				logrus.WithFields(logrus.Fields{
					"service":     serviceName,
					"statefulSet": statefulSetName,
					"replicas":    replicas,
				}).Debug("StatefulSet of Service Replicas")
				if replicas == int32(from) {
					if _, err := w.clientset.AppsV1().StatefulSets(service.Namespace).UpdateScale(ctx, statefulSetName, &autoscaling.Scale{
						ObjectMeta: meta.ObjectMeta{
							Name:            scale.Name,
							Namespace:       scale.Namespace,
							UID:             scale.UID,
							ResourceVersion: scale.ResourceVersion,
						},
						Spec: autoscaling.ScaleSpec{Replicas: int32(to)}}, meta.UpdateOptions{},
					); err == nil {
						logrus.WithFields(logrus.Fields{
							"service":     serviceName,
							"statefulSet": statefulSetName,
							"replicas":    replicas,
						}).Info("StatefulSet Replicas Autoscaled from "+strconv.Itoa(from)+" to "+strconv.Itoa(to))
					} else {
						return errors.Wrap(err, "UpdateScale for Replicas="+strconv.Itoa(to)+" failed for StatefulSet: "+statefulSetName)
					}
				}
			} else {
				return fmt.Errorf("GetScale failed for StatefulSet %s: %w", statefulSetName, err)
			}
		}
		return nil
	}
}
