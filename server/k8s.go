package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	AnnotationExternalServerName   = "mc-router.itzg.me/externalServerName"
	AnnotationDefaultServer        = "mc-router.itzg.me/defaultServer"
	AnnotationAutoScaleUp          = "mc-router.itzg.me/autoScaleUp"
	AnnotationAutoScaleDown        = "mc-router.itzg.me/autoScaleDown"
	AnnotationProxyServerName      = "mc-router.itzg.me/proxyServerName"
	AnnotationAutoScaleAsleepMOTD  = "mc-router.itzg.me/autoScaleAsleepMOTD"
	AnnotationAutoScaleLoadingMOTD = "mc-router.itzg.me/autoScaleLoadingMOTD"
	AnnotationAutoScaleWaitTimeout = "mc-router.itzg.me/autoScaleWaitTimeout"
)

// K8sWatcher is a RouteFinder that can find routes from kubernetes services.
// It also watches for stateful sets to auto-scale up/down, if enabled.
type K8sWatcher struct {
	sync.RWMutex
	config        *rest.Config
	autoScaleUp   bool
	autoScaleDown bool
	namespace     string
	// serviceStates entries are mutated in place and must only be accessed while holding the lock.
	serviceStates map[string]*serviceState // keyed by namespace + "/" + service name
	routesHandler RoutesHandler
	clientset     *kubernetes.Clientset
}

type serviceState struct {
	// statefulSetName is the StatefulSet governed by the Service; empty means no StatefulSet is known,
	// in which case routes keep the Service endpoint as their backend.
	statefulSetName string
	// ready is true when the known StatefulSet has at least one ready replica; only meaningful
	// when statefulSetName is set.
	ready bool
	// routes are the routes registered for the Service; they are blanked while the StatefulSet isn't ready.
	routes []*routableService
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

	w.Lock()
	w.serviceStates = make(map[string]*serviceState)
	w.Unlock()
	startServiceInformer := func() {
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
		logrus.Debug("Starting service informer")
		go serviceController.RunWithContext(ctx)
	}

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

		logrus.Debug("Running StatefulSet controller and service informer in background")
		go statefulSetController.RunWithContext(ctx)
		go func() {
			if cache.WaitForCacheSync(ctx.Done(), statefulSetController.HasSynced) {
				startServiceInformer()
			}
		}()
	} else {
		startServiceInformer()
	}

	logrus.Info("Monitoring Kubernetes for Minecraft services")
	return nil
}

func (w *K8sWatcher) handleAddStatefulSet() func(obj any) {
	return func(obj any) {
		statefulSet, ok := obj.(*apps.StatefulSet)
		if !ok {
			return
		}
		logrus.WithField("statefulSet", infoForSts(statefulSet)).
			Debug("ADD")
		serviceKey := k8sServiceKey(statefulSet.Namespace, statefulSet.Spec.ServiceName)
		w.Lock()
		state := w.getOrCreateServiceStateLocked(serviceKey)
		state.statefulSetName = statefulSet.Name
		state.ready = statefulSetHasReadyReplicas(statefulSet)
		logrus.
			WithField("serviceKey", serviceKey).
			WithField("statefulSetName", statefulSet.Name).
			WithField("ready", state.ready).
			Debug("Updating service state due to StatefulSet addition")
		w.Unlock()
		w.refreshServiceRoutes(serviceKey)
	}
}

func (w *K8sWatcher) handleUpdateStatefulSet() func(oldObj any, newObj any) {
	return func(oldObj, newObj any) {
		oldStatefulSet, ok := oldObj.(*apps.StatefulSet)
		if !ok {
			return
		}
		newStatefulSet, ok := newObj.(*apps.StatefulSet)
		if !ok {
			return
		}
		logrus.WithFields(logrus.Fields{"old": infoForSts(oldStatefulSet), "new": infoForSts(newStatefulSet)}).
			Debug("UPDATE")
		oldServiceKey := k8sServiceKey(oldStatefulSet.Namespace, oldStatefulSet.Spec.ServiceName)
		newServiceKey := k8sServiceKey(newStatefulSet.Namespace, newStatefulSet.Spec.ServiceName)
		newReady := statefulSetHasReadyReplicas(newStatefulSet)
		w.Lock()
		// Preserve routes already registered for this Service while updating the StatefulSet state.
		newState := w.getOrCreateServiceStateLocked(newServiceKey)
		wasKnown := newState.statefulSetName != ""
		oldReady := newState.ready
		if oldState, ok := w.serviceStates[oldServiceKey]; ok && oldServiceKey != newServiceKey && oldState.statefulSetName == oldStatefulSet.Name {
			// The StatefulSet changed its governing Service; mark the old key unknown but retain its routes.
			oldState.statefulSetName = ""
			oldState.ready = false
			logrus.
				WithField("oldServiceKey", oldServiceKey).
				Debug("Clearing STS name and readiness from service states due to StatefulSet changing its governing Service")
			w.deleteServiceStateIfUnusedLocked(oldServiceKey)
		}
		// Store the new name and readiness without replacing the Service's cached routes.
		newState.statefulSetName = newStatefulSet.Name
		newState.ready = newReady
		logrus.WithField("serviceKey", newServiceKey).
			WithField("statefulSetName", newState.statefulSetName).
			WithField("ready", newState.ready).
			Debug("Updating service state due to StatefulSet update")
		w.Unlock()
		if oldServiceKey != newServiceKey {
			// Refresh routes for the former Service after releasing the cache lock.
			w.refreshServiceRoutes(oldServiceKey)
		}
		if oldServiceKey != newServiceKey || !wasKnown || oldReady != newReady {
			// Avoid route updates for StatefulSet events that do not change routability.
			w.refreshServiceRoutes(newServiceKey)
		}
	}
}

func k8sServiceKey(namespace, serviceName string) string {
	return namespace + "/" + serviceName
}

// deleteServiceStateIfUnusedLocked removes cache entries once neither informer tracks them.
// The caller must hold w.Lock().
func (w *K8sWatcher) deleteServiceStateIfUnusedLocked(serviceKey string) {
	state, ok := w.serviceStates[serviceKey]
	if ok && state.statefulSetName == "" && len(state.routes) == 0 {
		logrus.WithField("serviceKey", serviceKey).Debug("Deleting unused service state")
		delete(w.serviceStates, serviceKey)
	}
}

// getOrCreateServiceStateLocked returns the mutable state for the key, creating it if needed.
// The caller must hold w.Lock().
func (w *K8sWatcher) getOrCreateServiceStateLocked(serviceKey string) *serviceState {
	if w.serviceStates == nil {
		w.serviceStates = make(map[string]*serviceState)
	}
	state, ok := w.serviceStates[serviceKey]
	if !ok {
		state = &serviceState{}
		w.serviceStates[serviceKey] = state
	}
	return state
}

func statefulSetHasReadyReplicas(statefulSet *apps.StatefulSet) bool {
	// Keep routes unavailable until a backend pod is ready, not merely requested.
	desiredReplicas := int32(1)
	if statefulSet.Spec.Replicas != nil {
		desiredReplicas = *statefulSet.Spec.Replicas
	}
	return desiredReplicas > 0 && statefulSet.Status.ReadyReplicas > 0
}

func (w *K8sWatcher) refreshServiceRoutes(serviceKey string) {
	logrus.WithField("serviceKey", serviceKey).Debug("Refreshing service routes")
	// Copy values under the lock since state is mutated in place by other handlers.
	var statefulSetName string
	var ready bool
	var routes []*routableService
	w.RLock()
	if state, ok := w.serviceStates[serviceKey]; ok {
		statefulSetName = state.statefulSetName
		ready = state.ready
		routes = append(routes, state.routes...)
	}
	w.RUnlock()

	if statefulSetName == "" {
		return
	}
	for _, routableService := range routes {
		backend := routableService.containerEndpoint
		if routableService.autoScaleUp != nil && !ready {
			backend = ""
		}
		w.updateRoute(routableService, backend)
	}
}

func (w *K8sWatcher) updateRoute(routableService *routableService, backend string) {
	logrus.WithField("backend", backend).
		WithField("externalServiceName", routableService.externalServiceName).
		Debug("Updating route")

	if routableService.externalServiceName != "" {
		w.routesHandler.UpdateMapping(
			routableService.externalServiceName,
			backend,
			routableService.scalingTarget,
			routableService.autoScaleUp,
			routableService.autoScaleDown,
			routableService.autoScaleAsleepMOTD,
			routableService.autoScaleLoadingMOTD,
		)
		return
	}
	w.routesHandler.SetDefaultRoute(
		backend,
		routableService.scalingTarget,
		routableService.autoScaleUp,
		routableService.autoScaleDown,
		routableService.autoScaleAsleepMOTD,
		routableService.autoScaleLoadingMOTD,
	)
}

func infoForSts(sts *apps.StatefulSet) string {
	return fmt.Sprintf("statefulSet{name=%s, replicas=%d, readyReplicas=%d, currentReplicas=%d, serviceName=%s}",
		sts.Name,
		sts.Status.Replicas,
		sts.Status.ReadyReplicas,
		sts.Status.CurrentReplicas,
		sts.Spec.ServiceName,
	)
}

func (w *K8sWatcher) handleDeleteStatefulSet() func(obj any) {
	return func(obj any) {
		statefulSet, ok := obj.(*apps.StatefulSet)
		if !ok {
			return
		}
		logrus.WithField("statefulSet", infoForSts(statefulSet)).
			Debug("DELETE")
		serviceKey := k8sServiceKey(statefulSet.Namespace, statefulSet.Spec.ServiceName)
		w.Lock()
		if state, ok := w.serviceStates[serviceKey]; ok && state.statefulSetName == statefulSet.Name {
			state.statefulSetName = ""
			state.ready = false
			logrus.
				WithField("serviceKey", serviceKey).
				Debug("Clearing STS name and readiness from service states due to StatefulSet deletion")
			w.deleteServiceStateIfUnusedLocked(serviceKey)
		}
		w.Unlock()
		w.refreshServiceRoutes(serviceKey)
	}
}

// oldObj and newObj are expected to be *v1.Service
func (w *K8sWatcher) handleUpdate(oldObj any, newObj any) {
	newServices := w.extractRoutableServices(newObj)
	newService, _ := newObj.(*core.Service)
	w.rememberServiceRoutes(newService, newServices)

	// Build a set of new service names for quick lookup
	newNames := make(map[string]struct{}, len(newServices))
	for _, rs := range newServices {
		if rs.externalServiceName != "" {
			newNames[rs.externalServiceName] = struct{}{}
		}
	}

	// Remove routes whose server address is no longer present in the new object
	for _, oldRS := range w.extractRoutableServices(oldObj) {
		logrus.WithFields(logrus.Fields{"old": oldRS}).Debug("UPDATE")
		if oldRS.externalServiceName != "" {
			if _, stillPresent := newNames[oldRS.externalServiceName]; !stillPresent {
				w.routesHandler.RemoveMapping(oldRS.externalServiceName)
			}
		}
	}

	// Update or create routes for new addresses; use UpdateMapping (no timer bounce)
	for _, newRS := range newServices {
		logrus.WithFields(logrus.Fields{"new": newRS}).Debug("UPDATE")
		if newRS.externalServiceName != "" {
			w.updateRoute(newRS, w.effectiveBackend(newRS))
		} else {
			w.updateRoute(newRS, w.effectiveBackend(newRS))
		}
	}
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) handleDelete(obj any) {
	service, _ := obj.(*core.Service)
	if service != nil {
		serviceKey := k8sServiceKey(service.Namespace, service.Name)
		w.Lock()
		if state, ok := w.serviceStates[serviceKey]; ok {
			state.routes = nil
			logrus.
				WithField("serviceKey", serviceKey).
				Debug("Removing routes from service states due to service deleted")
			w.deleteServiceStateIfUnusedLocked(serviceKey)
		}
		w.Unlock()
	}
	routableServices := w.extractRoutableServices(obj)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("DELETE")

			if routableService.externalServiceName != "" {
				w.routesHandler.RemoveMapping(routableService.externalServiceName)
			} else {
				w.routesHandler.RemoveDefaultRoute()
			}
		}
	}
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) handleAdd(obj any) {
	routableServices := w.extractRoutableServices(obj)
	service, _ := obj.(*core.Service)
	w.rememberServiceRoutes(service, routableServices)
	for _, routableService := range routableServices {
		if routableService != nil {
			logrus.WithField("routableService", routableService).Debug("ADD")

			if routableService.externalServiceName != "" {
				w.routesHandler.CreateMapping(routableService.externalServiceName, w.effectiveBackend(routableService), routableService.scalingTarget, routableService.autoScaleUp, routableService.autoScaleDown, routableService.autoScaleAsleepMOTD, routableService.autoScaleLoadingMOTD)
			} else {
				w.routesHandler.SetDefaultRoute(w.effectiveBackend(routableService), routableService.scalingTarget, routableService.autoScaleUp, routableService.autoScaleDown, routableService.autoScaleAsleepMOTD, routableService.autoScaleLoadingMOTD)
			}
		}
	}
}

func (w *K8sWatcher) rememberServiceRoutes(service *core.Service, routes []*routableService) {
	if service == nil {
		return
	}
	serviceKey := k8sServiceKey(service.Namespace, service.Name)
	w.Lock()
	w.getOrCreateServiceStateLocked(serviceKey).routes = routes
	logrus.
		WithField("serviceKey", serviceKey).
		Debug("Applying service routes to service state")
	w.Unlock()
}

func (w *K8sWatcher) effectiveBackend(routableService *routableService) string {
	if routableService.autoScaleUp == nil {
		return routableService.containerEndpoint
	}
	w.RLock()
	state, exists := w.serviceStates[routableService.serviceKey]
	asleep := exists && state.statefulSetName != "" && !state.ready
	w.RUnlock()
	if asleep {
		return ""
	}
	return routableService.containerEndpoint
}

type routableService struct {
	serviceKey           string
	externalServiceName  string
	containerEndpoint    string
	scalingTarget        ScalingTarget
	autoScaleUp          WakerFunc
	autoScaleDown        SleeperFunc
	autoScaleAsleepMOTD  string
	autoScaleLoadingMOTD string
}

func (r *routableService) String() string {
	return fmt.Sprintf("routableService{externalName=%s, endpoint=%s, scalingTarget=%s, autoScaleUp=%t, autoScaleDown=%t}",
		r.externalServiceName, r.containerEndpoint, r.scalingTarget, r.autoScaleUp != nil, r.autoScaleDown != nil)
}

// obj is expected to be a *v1.Service
func (w *K8sWatcher) extractRoutableServices(obj any) []*routableService {
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

	routingEndpoint := endpoint

	if proxyServerName, exists := service.Annotations[AnnotationProxyServerName]; exists && proxyServerName != "" {
		// Ensure the proxy address has a port
		if _, _, err := net.SplitHostPort(proxyServerName); err != nil {
			proxyServerName = net.JoinHostPort(proxyServerName, "25565")
		}
		routingEndpoint = proxyServerName
		// scalingTarget remains the service endpoint (already set above)
	}
	scalingTarget := NewK8sScalingTarget(service.Namespace, service.Name)

	autoScaleAsleepMOTD := ""
	autoScaleLoadingMOTD := ""
	waitTimeout := 60 * time.Second

	if asleepMOTD, exists := service.Annotations[AnnotationAutoScaleAsleepMOTD]; exists && asleepMOTD != "" {
		autoScaleAsleepMOTD = asleepMOTD
	}

	if loadingMOTD, exists := service.Annotations[AnnotationAutoScaleLoadingMOTD]; exists && loadingMOTD != "" {
		autoScaleLoadingMOTD = loadingMOTD
	}

	if timeoutStr, exists := service.Annotations[AnnotationAutoScaleWaitTimeout]; exists && timeoutStr != "" {
		if parsedTimeout, err := time.ParseDuration(timeoutStr); err == nil && parsedTimeout > 0 {
			waitTimeout = parsedTimeout
		} else {
			logrus.WithError(err).WithField("annotation", AnnotationAutoScaleWaitTimeout).WithField("value", timeoutStr).Warn("Invalid wait timeout annotation, falling back to default 60s")
		}
	}

	wakerFunc := w.buildScaleFunction(service, 0, 1)
	rs := &routableService{
		serviceKey:           k8sServiceKey(service.Namespace, service.Name),
		externalServiceName:  externalServiceName,
		containerEndpoint:    routingEndpoint,
		scalingTarget:        scalingTarget,
		autoScaleUp:          buildK8sWaker(routingEndpoint, wakerFunc, waitTimeout),
		autoScaleDown:        w.buildScaleFunction(service, 1, 0),
		autoScaleAsleepMOTD:  autoScaleAsleepMOTD,
		autoScaleLoadingMOTD: autoScaleLoadingMOTD,
	}
	return rs
}

func buildK8sWaker(endpoint string, scaleUp SleeperFunc, waitTimeout time.Duration) WakerFunc {
	if scaleUp == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		// The connector already holds the StartScaling lock before calling the waker,
		// so we go straight to scaling up without a redundant StartScaling check.
		logrus.
			WithField("endpoint", endpoint).
			Debug("Scaling up K8s StatefulSet replicas to 1")
		if err := scaleUp(ctx); err != nil {
			return "", err
		}

		logrus.
			WithField("endpoint", endpoint).
			WithField("waitTimeout", waitTimeout).
			Debug("Waiting for K8s backend to become reachable")

		return waitForBackend(ctx, endpoint, waitTimeout)
	}
}

// buildScaleFunction generates a SleeperFunc to scale StatefulSets based on specified criteria and service annotations.
// Will return nil if the service should not be auto-scaled due config or annotation.
func (w *K8sWatcher) buildScaleFunction(service *core.Service, from int32, to int32) SleeperFunc {
	// Currently, annotations can only be used to opt out of auto-scaling.
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

	logrus.WithField("service", service.Name).
		WithField("from", from).
		WithField("to", to).
		Debug("Service auto-scale enabled")
	return func(ctx context.Context) error {
		serviceName := service.Name
		serviceKey := k8sServiceKey(service.Namespace, service.Name)
		w.RLock()
		var statefulSetName string
		if state, exists := w.serviceStates[serviceKey]; exists {
			statefulSetName = state.statefulSetName
		}
		w.RUnlock()
		if statefulSetName != "" {
			logrus.
				WithField("service", serviceName).
				WithField("from", from).
				WithField("to", to).
				WithField("statefulSet", statefulSetName).
				Debug("Scaling StatefulSet")

			// Get current replicas to check if scaling is needed
			if scale, err := w.clientset.AppsV1().StatefulSets(service.Namespace).GetScale(ctx, statefulSetName, meta.GetOptions{}); err == nil {
				replicas := scale.Status.Replicas
				logrus.WithFields(logrus.Fields{
					"service":     serviceName,
					"statefulSet": statefulSetName,
					"replicas":    replicas,
				}).Debug("StatefulSet of Service Replicas")

				if replicas == from {
					// Use Patch instead of Update to avoid optimistic concurrency errors
					// This doesn't require resourceVersion and is atomic
					patchData := fmt.Sprintf(`{"spec":{"replicas":%d}}`, to)
					_, err := w.clientset.AppsV1().StatefulSets(service.Namespace).Patch(
						ctx,
						statefulSetName,
						types.StrategicMergePatchType,
						[]byte(patchData),
						meta.PatchOptions{},
					)
					if err == nil {
						logrus.WithFields(logrus.Fields{
							"service":     serviceName,
							"statefulSet": statefulSetName,
							"replicas":    replicas,
						}).Infof("StatefulSet Replicas Autoscaled from %d to %d", from, to)
						return nil
					}

					// Fallback to UpdateScale if Patch fails due to RBAC permissions
					// This maintains backward compatibility with existing RBAC configurations
					if strings.Contains(err.Error(), "forbidden") {
						logrus.WithFields(logrus.Fields{
							"service":     serviceName,
							"statefulSet": statefulSetName,
						}).Warn("Patch operation forbidden - falling back to UpdateScale. Consider updating RBAC to allow 'patch' verb for better concurrency handling")

						scale.Spec.Replicas = to
						if _, updateErr := w.clientset.AppsV1().StatefulSets(service.Namespace).UpdateScale(
							ctx,
							statefulSetName,
							scale,
							meta.UpdateOptions{},
						); updateErr == nil {
							logrus.WithFields(logrus.Fields{
								"service":     serviceName,
								"statefulSet": statefulSetName,
								"replicas":    replicas,
							}).Infof("StatefulSet Replicas Autoscaled from %d to %d (via UpdateScale fallback)", from, to)
							return nil
						} else {
							return errors.Wrapf(updateErr, "UpdateScale fallback for Replicas=%d failed for StatefulSet: %s", to, statefulSetName)
						}
					}

					return errors.Wrapf(err, "Patch for Replicas=%d failed for StatefulSet: %s", to, statefulSetName)
				}
				// Replicas already at desired state
				return nil
			} else {
				return fmt.Errorf("GetScale failed for StatefulSet %s: %w", statefulSetName, err)
			}
		}
		logrus.WithField("service", serviceName).Warn("Service has no StatefulSet associated - skipping scaling")
		return nil
	}
}

type K8sScalingTarget struct {
	ScalingIndicator
	namespace   string
	serviceName string
}

func NewK8sScalingTarget(namespace string, serviceName string) *K8sScalingTarget {
	return &K8sScalingTarget{
		ScalingIndicator: ScalingIndicator{scaling: &atomic.Bool{}},
		namespace:        namespace,
		serviceName:      serviceName,
	}
}

func (t *K8sScalingTarget) String() string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("k8s{serviceName=%s, namespace=%s}", t.serviceName, t.namespace)
}

func (t *K8sScalingTarget) ScalingKey() string {
	return t.namespace + ":" + t.serviceName
}
