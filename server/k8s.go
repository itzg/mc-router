package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/itzg/mc-router/mcproto"
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
	AnnotationBetaServerVersion  = "mc-router.itzg.me/betaServerVersion"
	AnnotationDefaultServer      = "mc-router.itzg.me/defaultServer"
	AnnotationAutoScaleUp        = "mc-router.itzg.me/autoScaleUp"
	AnnotationAutoScaleDown      = "mc-router.itzg.me/autoScaleDown"
)

// K8sWatcher is a RouteFinder that can find routes from kubernetes services.
// It also watches for stateful sets to auto scale up/down, if enabled.
type K8sWatcher struct {
	sync.RWMutex
	config        *rest.Config
	autoScaleUp   bool
	autoScaleDown bool
	namespace     string
	// The key in mappings is a Service, and the value the StatefulSet name
	mappings      map[string]string
	routesHandler RoutesHandler
	clientset     *kubernetes.Clientset
}

func NewK8sWatcherInCluster() (*K8sWatcher, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(err, "Unable to load in-cluster config")
	}

	return &K8sWatcher{
		config: config,
	}, nil
}

func NewK8sWatcherWithConfig(kubeConfigFile string) (*K8sWatcher, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigFile)
	if err != nil {
		return nil, errors.Wrap(err, "Could not load kube config file")
	}

	return &K8sWatcher{
		config: config,
	}, nil
}

func (w *K8sWatcher) WithAutoScale(autoScaleUp bool, autoScaleDown bool) *K8sWatcher {
	w.autoScaleUp = autoScaleUp
	w.autoScaleDown = autoScaleDown
	return w
}

func (w *K8sWatcher) WithNamespace(namespace string) *K8sWatcher {
	w.namespace = namespace
	return w
}

func (w *K8sWatcher) String() string {
	return "k8s"
}

func (w *K8sWatcher) Start(ctx context.Context, handler RoutesHandler) error {
	w.routesHandler = handler
	clientset, err := kubernetes.NewForConfig(w.config)
	if err != nil {
		return errors.Wrap(err, "Could not create kube clientset")
	}
	w.clientset = clientset

	_, serviceController := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: cache.NewListWatchFromClient(
			clientset.CoreV1().RESTClient(),
			string(core.ResourceServices),
			w.namespace,
			fields.Everything(),
		),
		ObjectType: &core.Service{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    w.handleAdd,
			DeleteFunc: w.handleDelete,
			UpdateFunc: w.handleUpdate,
		},
	})
	go serviceController.RunWithContext(ctx)

	w.mappings = make(map[string]string)
	if w.autoScaleUp || w.autoScaleDown {
		_, statefulSetController := cache.NewInformerWithOptions(cache.InformerOptions{
			ListerWatcher: cache.NewListWatchFromClient(
				clientset.AppsV1().RESTClient(),
				"statefulSets",
				w.namespace,
				fields.Everything(),
			),
			ObjectType: &apps.StatefulSet{},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    w.handleAddStatefulSet(),
				DeleteFunc: w.handleDeleteStatefulSet(),
				UpdateFunc: w.handleUpdateStatefulSet(),
			},
		})

		go statefulSetController.RunWithContext(ctx)
	}

	logrus.Info("Monitoring Kubernetes for Minecraft services")
	return nil
}

func (w *K8sWatcher) handleAddStatefulSet() func(obj interface{}) {
	return func(obj interface{}) {
		statefulSet, ok := obj.(*apps.StatefulSet)
		if !ok {
			return
		}
		w.RLock()
		defer w.RUnlock()
		w.mappings[statefulSet.Spec.ServiceName] = statefulSet.Name
	}
}

func (w *K8sWatcher) handleUpdateStatefulSet() func(oldObj interface{}, newObj interface{}) {
	return func(oldObj, newObj interface{}) {
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
	}
}

func (w *K8sWatcher) handleDeleteStatefulSet() func(obj interface{}) {
	return func(obj interface{}) {
		statefulSet, ok := obj.(*apps.StatefulSet)
		if !ok {
			return
		}
		w.RLock()
		defer w.RUnlock()
		delete(w.mappings, statefulSet.Spec.ServiceName)
	}
}

// oldObj and newObj are expected to be *v1.Service
func (w *K8sWatcher) handleUpdate(oldObj interface{}, newObj interface{}) {
	for _, oldRoutableService := range w.extractRoutableServices(oldObj) {
		logrus.WithFields(logrus.Fields{
			"old": oldRoutableService,
		}).Debug("UPDATE")
		if oldRoutableService.externalServiceName != "" {
			w.routesHandler.DeleteMapping(oldRoutableService.externalServiceName)
		}
	}

	for _, newRoutableService := range w.extractRoutableServices(newObj) {
		logrus.WithFields(logrus.Fields{
			"new": newRoutableService,
		}).Debug("UPDATE")
		if newRoutableService.externalServiceName != "" {
			w.routesHandler.CreateMapping(newRoutableService.externalServiceName, newRoutableService.containerEndpoint, newRoutableService.autoScaleUp, newRoutableService.autoScaleDown, "")
		} else {
			w.routesHandler.SetDefaultRoute(newRoutableService.containerEndpoint, newRoutableService.autoScaleUp, newRoutableService.autoScaleDown, "")
		}
	}
	for _, oldRoutableBetaService := range w.extractRoutableBetaServices(oldObj) {
		logrus.WithFields(logrus.Fields{
			"old": oldRoutableBetaService,
		}).Debug("UPDATE BETA")
		if oldRoutableBetaService.externalServiceProtocolNumber != 0 {
			w.routesHandler.CreateBetaMapping(oldRoutableBetaService.externalServiceProtocolNumber, "", nil, nil, "")
		}
	}

	for _, newRoutableBetaService := range w.extractRoutableBetaServices(newObj) {
		logrus.WithFields(logrus.Fields{
			"new": newRoutableBetaService,
		}).Debug("UPDATE BETA")
		if newRoutableBetaService.externalServiceProtocolNumber != 0 {
			w.routesHandler.CreateBetaMapping(newRoutableBetaService.externalServiceProtocolNumber, newRoutableBetaService.containerEndpoint, newRoutableBetaService.autoScaleUp, newRoutableBetaService.autoScaleDown, "")
		} else { //TODO default beta route
			w.routesHandler.SetDefaultRoute(newRoutableBetaService.containerEndpoint, newRoutableBetaService.autoScaleUp, newRoutableBetaService.autoScaleDown, "")
		}
	}
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) handleDelete(obj interface{}) {
	routableServices := w.extractRoutableServices(obj)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("DELETE")

			if routableService.externalServiceName != "" {
				w.routesHandler.DeleteMapping(routableService.externalServiceName)
			} else {
				w.routesHandler.SetDefaultRoute("", nil, nil, "")
			}
		}
	}
	for _, routableBetaService := range w.extractRoutableBetaServices(obj) {
		if routableBetaService != nil {
			logrus.WithField("routableBetaService", routableBetaService).Debug("DELETE BETA")

			if routableBetaService.externalServiceProtocolNumber != 0 {
				w.routesHandler.CreateBetaMapping(routableBetaService.externalServiceProtocolNumber, "", nil, nil, "")
			} else { //TODO default beta route
				w.routesHandler.SetDefaultRoute("", nil, nil, "")
			}
		}
	}
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) handleAdd(obj interface{}) {
	routableServices := w.extractRoutableServices(obj)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("ADD")

			if routableService.externalServiceName != "" {
				w.routesHandler.CreateMapping(routableService.externalServiceName, routableService.containerEndpoint, routableService.autoScaleUp, routableService.autoScaleDown, "")
			} else { //TODO default beta route
				w.routesHandler.SetDefaultRoute(routableService.containerEndpoint, routableService.autoScaleUp, routableService.autoScaleDown, "")
			}
		}
	}
	routableBetaServices := w.extractRoutableBetaServices(obj)
	for _, routableBetaService := range routableBetaServices {
		if routableBetaService != nil {
			logrus.WithField("routableBetaService", routableBetaService).Debug("ADD BETA")

			w.routesHandler.CreateBetaMapping(routableBetaService.externalServiceProtocolNumber, routableBetaService.containerEndpoint, routableBetaService.autoScaleUp, routableBetaService.autoScaleDown, "")
		}
	}
}

type routableService struct {
	externalServiceName string
	containerEndpoint   string
	autoScaleUp         WakerFunc
	autoScaleDown       SleeperFunc
}

type routableBetaService struct {
	externalServiceProtocolNumber int
	containerEndpoint             string
	autoScaleUp                   WakerFunc
	autoScaleDown                 SleeperFunc
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) extractRoutableServices(obj interface{}) []*routableService {
	service, ok := obj.(*core.Service)
	if !ok {
		return nil
	}

	routableServices := make([]*routableService, 0)
	if externalServiceName, exists := service.Annotations[AnnotationExternalServerName]; exists {
		serviceNames := SplitExternalHosts(externalServiceName)
		for _, serviceName := range serviceNames {
			routableServices = append(routableServices, w.buildDetails(service, serviceName))
		}
		return routableServices
	} else if _, exists := service.Annotations[AnnotationDefaultServer]; exists {
		return []*routableService{w.buildDetails(service, "")}
	}

	return nil
}

func (w *K8sWatcher) extractRoutableBetaServices(obj interface{}) []*routableBetaService {
	service, ok := obj.(*core.Service)
	if !ok {
		return nil
	}

	routableBetaServices := make([]*routableBetaService, 0)
	if betaServerVersion, exists := service.Annotations[AnnotationBetaServerVersion]; exists {
		versionStrings := strings.Split(betaServerVersion, ",")
		for _, versionString := range versionStrings {
			versionProtocol := mcproto.VersionToProtocolVersion(versionString)
			if versionProtocol != -1 {
				routableBetaServices = append(routableBetaServices, w.buildBetaDetails(service, versionProtocol))
			} else {
				logrus.WithFields(logrus.Fields{
					"service": service.Name,
					"version": versionString,
				}).Warn("Ignoring invalid beta server version annotation")
			}
		}
		return routableBetaServices
	} else if _, exists := service.Annotations[AnnotationDefaultServer]; exists {
		return []*routableBetaService{w.buildBetaDetails(service, 0)}
	}

	return nil
}

func (w *K8sWatcher) buildDetails(service *core.Service, externalServiceName string) *routableService {
	clusterIp := service.Spec.ClusterIP
	if service.Spec.Type == core.ServiceTypeExternalName {
		clusterIp = service.Spec.ExternalName
	}
	mcRouterPort := ""
	mcPort := ""
	for _, p := range service.Spec.Ports {
		if p.Name == "mc-router" {
			mcRouterPort = strconv.Itoa(int(p.Port))
		}
		if p.Name == "minecraft" {
			mcPort = strconv.Itoa(int(p.Port))
		}
	}
	port := "25565"
	if len(mcRouterPort) > 0 {
		port = mcRouterPort
	} else if len(mcPort) > 0 {
		port = mcPort
	}
	endpoint := net.JoinHostPort(clusterIp, port)
	wakerFunc := w.buildScaleFunction(service, 0, 1)
	rs := &routableService{
		externalServiceName: externalServiceName,
		containerEndpoint:   endpoint,
		autoScaleUp:         buildWakerFromSleeper(endpoint, wakerFunc),
		autoScaleDown:       w.buildScaleFunction(service, 1, 0),
	}
	return rs
}

func (w *K8sWatcher) buildBetaDetails(service *core.Service, externalServiceProtocolNumber int) *routableBetaService {
	clusterIp := service.Spec.ClusterIP
	if service.Spec.Type == core.ServiceTypeExternalName {
		clusterIp = service.Spec.ExternalName
	}
	mcRouterPort := ""
	mcPort := ""
	for _, p := range service.Spec.Ports {
		if p.Name == "mc-router" {
			mcRouterPort = strconv.Itoa(int(p.Port))
		}
		if p.Name == "minecraft" {
			mcPort = strconv.Itoa(int(p.Port))
		}
	}
	port := "25565"
	if len(mcRouterPort) > 0 {
		port = mcRouterPort
	} else if len(mcPort) > 0 {
		port = mcPort
	}
	endpoint := net.JoinHostPort(clusterIp, port)
	wakerFunc := w.buildScaleFunction(service, 0, 1)
	rs := &routableBetaService{
		externalServiceProtocolNumber: externalServiceProtocolNumber,
		containerEndpoint:             endpoint,
		autoScaleUp:                   buildWakerFromSleeper(endpoint, wakerFunc),
		autoScaleDown:                 w.buildScaleFunction(service, 1, 0),
	}
	return rs
}

// buildScaleFunction generates a SleeperFunc to scale StatefulSets based on specified criteria and service annotations.
// Will return nil if the service should not be auto-scaled due config or annotation.
func (w *K8sWatcher) buildScaleFunction(service *core.Service, from int32, to int32) SleeperFunc {
	// Currently, annotations can only be used to opt-out of auto-scaling.
	// However, this logic is prepared also for opt-in, as it returns a `SleeperFunc` when flags are false but annotations are set to `enabled`.
	if from <= to {
		enabled, exists := service.Annotations[AnnotationAutoScaleUp]
		if exists {
			enabledBool, err := strconv.ParseBool(strings.TrimSpace(enabled))
			if err != nil {
				logrus.WithFields(logrus.Fields{"service": service.Name}).
					WithError(err).
					Warnf("invalid value for %s annotation - disabling service auto-scale-up", AnnotationAutoScaleUp)
				return nil
			}

			if !enabledBool {
				return nil
			}
		} else {
			if !w.autoScaleUp {
				return nil
			}
		}
	}
	if from >= to {
		enabled, exists := service.Annotations[AnnotationAutoScaleDown]
		if exists {
			enabledBool, err := strconv.ParseBool(strings.TrimSpace(enabled))
			if err != nil {
				logrus.WithFields(logrus.Fields{"service": service.Name}).
					WithError(err).
					Warnf("invalid value for %s annotation - disabling service auto-scale-down", AnnotationAutoScaleDown)
				return nil
			}

			if !enabledBool {
				return nil
			}
		} else {
			if !w.autoScaleDown {
				return nil
			}
		}

	}
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
				if replicas == from {
					if _, err := w.clientset.AppsV1().StatefulSets(service.Namespace).UpdateScale(ctx, statefulSetName, &autoscaling.Scale{
						ObjectMeta: meta.ObjectMeta{
							Name:            scale.Name,
							Namespace:       scale.Namespace,
							UID:             scale.UID,
							ResourceVersion: scale.ResourceVersion,
						},
						Spec: autoscaling.ScaleSpec{Replicas: to}}, meta.UpdateOptions{},
					); err == nil {
						logrus.WithFields(logrus.Fields{
							"service":     serviceName,
							"statefulSet": statefulSetName,
							"replicas":    replicas,
						}).Infof("StatefulSet Replicas Autoscaled from %d to %d", from, to)
					} else {
						return errors.Wrapf(err, "UpdateScale for Replicas=%d failed for StatefulSet: %s", to, statefulSetName)
					}
				}
			} else {
				return fmt.Errorf("GetScale failed for StatefulSet %s: %w", statefulSetName, err)
			}
		}
		return nil
	}
}
