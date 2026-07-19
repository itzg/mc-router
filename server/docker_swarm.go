package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

func NewDockerSwarmWatcher(socket string, timeout time.Duration, autoScaleUp bool, autoScaleDown bool, dockerApiVersion string, routes IRoutes) IDockerWatcher {
	return &dockerSwarmWatcherImpl{
		config: dockerWatcherConfig{
			socket:        socket,
			timeout:       timeout,
			autoScaleUp:   autoScaleUp,
			autoScaleDown: autoScaleDown,
			apiVersion:    dockerApiVersion,
		},
		routes: routes,
	}
}

type routableSwarmService struct {
	externalServiceName       string
	containerEndpoint         string
	serviceID                 string
	serviceName               string
	networkID                 string
	autoScaleUp               bool
	autoScaleDown             bool
	autoScaleAsleepMOTD       string
	autoScaleLoadingMOTD      string
	autoScaleWaitTimeout      time.Duration
	autoScaleFailedMOTD       string
	autoScaleRestartDelayMOTD string
	countdownDeadline         time.Time
	statusState               string
}

type dockerSwarmWatcherImpl struct {
	sync.RWMutex
	config      dockerWatcherConfig
	client      *client.Client
	serviceMap  map[string]*routableSwarmService
	monitorLock sync.Mutex
	routes      IRoutes
}

func (w *dockerSwarmWatcherImpl) makeWakerFunc(rs *routableSwarmService) WakerFunc {
	if rs == nil || !rs.autoScaleUp {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		serviceID := rs.serviceID
		if serviceID == "" {
			return "", fmt.Errorf("missing service id for wake")
		}

		service, _, err := w.client.ServiceInspectWithRaw(ctx, serviceID, dockertypes.ServiceInspectOptions{})
		if err != nil {
			return "", err
		}

		if service.Spec.Mode.Replicated == nil {
			return "", fmt.Errorf("service %s is not replicated and cannot be scaled", serviceID)
		}

		var delay time.Duration
		if service.Spec.TaskTemplate.RestartPolicy != nil {
			if service.Spec.TaskTemplate.RestartPolicy.Delay != nil {
				delay = *service.Spec.TaskTemplate.RestartPolicy.Delay
			}
		}

		waitTimeout := rs.autoScaleWaitTimeout
		if waitTimeout == 0 {
			waitTimeout = 60 * time.Second
		}

		if service.Spec.Mode.Replicated != nil && service.Spec.Mode.Replicated.Replicas != nil && *service.Spec.Mode.Replicated.Replicas == 0 {
			logrus.WithFields(logrus.Fields{
				"serviceID":   serviceID,
				"serviceName": rs.serviceName,
			}).Debug("Scaling up Swarm service to 1 replica")
			one := uint64(1)
			service.Spec.Mode.Replicated.Replicas = &one

			_, err = w.client.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, dockertypes.ServiceUpdateOptions{})
			if err != nil {
				return "", err
			}
		}

		// Wait until a task is running and has an IP address
		var taskIP string
		deadline := time.Now().Add(waitTimeout)
		for {
			tasks, err := w.client.TaskList(ctx, dockertypes.TaskListOptions{
				Filters: filters.NewArgs(filters.Arg("service", serviceID)),
			})
			if err == nil && len(tasks) > 0 {
				var hasActiveTask bool
				var hasReadyTask bool
				var readyTaskTimestamp time.Time
				var latestTask swarm.Task

				for _, task := range tasks {
					state := task.Status.State
					desiredState := task.DesiredState

					// Track the most recently created task to inspect its DesiredState.
					if latestTask.CreatedAt.IsZero() || task.CreatedAt.After(latestTask.CreatedAt) {
						latestTask = task
					}

					// The task is considered fully Running only if both actual and desired states are running.
					if state == swarm.TaskStateRunning && desiredState == swarm.TaskStateRunning {
						for _, attachment := range task.NetworksAttachments {
							matchesNetwork := rs.networkID != "" && attachment.Network.ID == rs.networkID
							isIngress := attachment.Network.Spec.Name == "ingress"

							if (matchesNetwork || (rs.networkID == "" && !isIngress)) && len(attachment.Addresses) > 0 {
								parts := strings.Split(attachment.Addresses[0], "/")
								if ip := net.ParseIP(parts[0]); ip != nil {
									taskIP = parts[0]
									break
								}
							}
						}
					}

					// Swarm task state 'ready' (actual or desired) marks a task held during a restart delay.
					// Find the latest ready task's status timestamp to measure the restart delay start.
					if state == swarm.TaskStateReady || desiredState == swarm.TaskStateReady {
						hasReadyTask = true
						if task.Status.Timestamp.After(readyTaskTimestamp) {
							readyTaskTimestamp = task.Status.Timestamp
						}
					}

					// Track active task states to see if Swarm is actively attempting to schedule/start a task.
					if state == swarm.TaskStateNew ||
						state == swarm.TaskStatePending ||
						state == swarm.TaskStateAssigned ||
						state == swarm.TaskStateAccepted ||
						state == swarm.TaskStatePreparing ||
						state == swarm.TaskStateReady ||
						state == swarm.TaskStateStarting ||
						state == swarm.TaskStateRunning ||
						desiredState == swarm.TaskStateReady ||
						desiredState == swarm.TaskStateRunning {
						hasActiveTask = true
					}
				}

				if taskIP != "" {
					break
				}

				// Check if Swarm gave up or is in restart delay
				swarmGaveUp := false
				var remainingDelay time.Duration

				if hasReadyTask && delay > 0 {
					// Waker is waiting for a restart delay to expire. Dynamically extend the deadline
					// so we do not timeout the connection while Swarm holds the start attempt.
					if !readyTaskTimestamp.IsZero() && time.Since(readyTaskTimestamp) < delay {
						timeSinceReady := time.Since(readyTaskTimestamp)
						remainingDelay = delay - timeSinceReady
						newDeadline := readyTaskTimestamp.Add(delay).Add(waitTimeout)
						if newDeadline.After(deadline) {
							deadline = newDeadline
							logrus.WithFields(logrus.Fields{
								"service":      serviceID,
								"remaining":    remainingDelay,
								"extendedWait": time.Until(deadline),
							}).Info("Swarm task is in restart delay. Dynamically extending waker deadline.")
						}
					}
				} else if !hasActiveTask && latestTask.DesiredState == swarm.TaskStateShutdown && len(tasks) > 0 {
					// Mentality: If the latest task has DesiredState == Shutdown and there are no active tasks,
					// Swarm has given up retrying.
					swarmGaveUp = true
				}

				if swarmGaveUp {
					return "", fmt.Errorf("Swarm has stopped attempting to start service %s: all tasks have terminated", serviceID)
				}
			}
			if taskIP != "" {
				break
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for running task for service %s", serviceID)
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		_, portStr, err := net.SplitHostPort(rs.containerEndpoint)
		if err != nil {
			portStr = "25565"
		}
		endpoint := net.JoinHostPort(taskIP, portStr)

		// Wait for the task endpoint to be reachable
		for {
			conn, err := net.DialTimeout("tcp", endpoint, 1*time.Second)
			if err == nil {
				_ = conn.Close()
				break
			}
			if ctx.Err() != nil {
				return endpoint, ctx.Err()
			}
			if time.Now().After(deadline) {
				return endpoint, fmt.Errorf("timeout waiting for Swarm service task to become reachable at %s", endpoint)
			}
			select {
			case <-ctx.Done():
				return endpoint, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		return endpoint, nil
	}
}

func (w *dockerSwarmWatcherImpl) makeSleeperFunc(rs *routableSwarmService) SleeperFunc {
	if rs == nil || !rs.autoScaleDown {
		return nil
	}
	return func(ctx context.Context) error {
		serviceID := rs.serviceID
		if serviceID == "" {
			return fmt.Errorf("missing service id for sleep")
		}

		service, _, err := w.client.ServiceInspectWithRaw(ctx, serviceID, dockertypes.ServiceInspectOptions{})
		if err != nil {
			return err
		}

		if service.Spec.Mode.Replicated == nil {
			return fmt.Errorf("service %s is not replicated and cannot be scaled", serviceID)
		}

		replicas := service.Spec.Mode.Replicated.Replicas
		if replicas != nil && *replicas > 0 {
			logrus.WithFields(logrus.Fields{
				"serviceID":   serviceID,
				"serviceName": rs.serviceName,
			}).Debug("Scaling down Swarm service to 0 replicas")
			zero := uint64(0)
			service.Spec.Mode.Replicated.Replicas = &zero

			_, err = w.client.ServiceUpdate(ctx, serviceID, service.Version, service.Spec, dockertypes.ServiceUpdateOptions{})
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func (w *dockerSwarmWatcherImpl) Start(ctx context.Context) error {
	var err error

	opts := []client.Opt{
		client.WithHost(w.config.socket),
		client.WithTimeout(w.config.timeout),
		client.WithHTTPHeaders(map[string]string{
			"User-Agent": "mc-router ",
		}),
		client.WithAPIVersionNegotiation(),
	}

	w.client, err = client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}

	w.serviceMap = map[string]*routableSwarmService{}

	logrus.Trace("Performing initial listing of Docker swarm services")
	if err := w.reconcileServices(ctx); err != nil {
		return err
	}

	go w.streamEvents(ctx)

	logrus.Info("Monitoring Docker Swarm for Minecraft services")
	return nil
}

func (w *dockerSwarmWatcherImpl) reconcileServices(ctx context.Context) error {
	w.monitorLock.Lock()
	defer w.monitorLock.Unlock()

	services, err := w.listServices(ctx)
	if err != nil {
		logrus.WithError(err).Error("Docker failed to list services")
		return err
	}

	visited := map[string]struct{}{}
	for _, rs := range services {
		// If this is a newly discovered service, set up wakers/sleepers and create the route mapping.
		if oldRs, ok := w.serviceMap[rs.externalServiceName]; !ok {
			w.serviceMap[rs.externalServiceName] = rs
			ipDetail := ""
			if rs.statusState == "running" && rs.containerEndpoint != "" {
				ipDetail = fmt.Sprintf(" (Endpoint: %s)", rs.containerEndpoint)
			}
			logrus.WithFields(logrus.Fields{
				"service": rs.serviceName,
				"hosts":   rs.externalServiceName,
			}).Infof("Swarm service state: %s%s", rs.statusState, ipDetail)

			wakerFunc := w.makeWakerFunc(rs)
			sleeperFunc := w.makeSleeperFunc(rs)
			if rs.externalServiceName != "" {
				w.routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, rs.serviceID, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			} else {
				w.routes.SetDefaultRoute(rs.containerEndpoint, rs.serviceID, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			}
			w.routes.SetCountdownDeadline(rs.externalServiceName, rs.countdownDeadline)
			// If the service is already tracked, check if any metadata, endpoint, MOTDs, or deadline
			// changed. If so, recreate wakers/sleepers and update the route table.
		} else if oldRs.containerEndpoint != rs.containerEndpoint ||
			oldRs.serviceID != rs.serviceID ||
			oldRs.networkID != rs.networkID ||
			oldRs.autoScaleUp != rs.autoScaleUp ||
			oldRs.autoScaleDown != rs.autoScaleDown ||
			oldRs.autoScaleAsleepMOTD != rs.autoScaleAsleepMOTD ||
			oldRs.autoScaleLoadingMOTD != rs.autoScaleLoadingMOTD ||
			oldRs.autoScaleWaitTimeout != rs.autoScaleWaitTimeout ||
			oldRs.autoScaleFailedMOTD != rs.autoScaleFailedMOTD ||
			oldRs.autoScaleRestartDelayMOTD != rs.autoScaleRestartDelayMOTD ||
			oldRs.countdownDeadline != rs.countdownDeadline ||
			oldRs.statusState != rs.statusState {

			if oldRs.statusState != rs.statusState {
				ipDetail := ""
				if rs.statusState == "running" && rs.containerEndpoint != "" {
					ipDetail = fmt.Sprintf(" (Endpoint: %s)", rs.containerEndpoint)
				}
				logrus.WithFields(logrus.Fields{
					"service": rs.serviceName,
					"hosts":   rs.externalServiceName,
				}).Infof("Swarm service state transition: %s -> %s%s", oldRs.statusState, rs.statusState, ipDetail)
			}

			w.serviceMap[rs.externalServiceName] = rs
			wakerFunc := w.makeWakerFunc(rs)
			sleeperFunc := w.makeSleeperFunc(rs)
			if rs.externalServiceName != "" {
				w.routes.DeleteMapping(rs.externalServiceName)
				w.routes.CreateMapping(rs.externalServiceName, rs.containerEndpoint, rs.serviceID, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			} else {
				w.routes.SetDefaultRoute(rs.containerEndpoint, rs.serviceID, wakerFunc, sleeperFunc, rs.autoScaleAsleepMOTD, rs.autoScaleLoadingMOTD)
			}
			w.routes.SetCountdownDeadline(rs.externalServiceName, rs.countdownDeadline)
			logrus.WithFields(logrus.Fields{"old": oldRs, "new": rs}).Debug("UPDATE")
		}
		visited[rs.externalServiceName] = struct{}{}
	}
	for _, rs := range w.serviceMap {
		if _, ok := visited[rs.externalServiceName]; !ok {
			delete(w.serviceMap, rs.externalServiceName)
			if rs.externalServiceName != "" {
				w.routes.DeleteMapping(rs.externalServiceName)
			} else {
				w.routes.SetDefaultRoute("", "", nil, nil, "", "")
			}
			logrus.WithField("routableService", rs).Debug("DELETE")
		}
	}
	return nil
}

func (w *dockerSwarmWatcherImpl) streamEvents(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			logrus.Debug("Stopping Docker Swarm monitoring")
			return
		}

		eventFilters := filters.NewArgs(
			filters.Arg("type", string(events.ServiceEventType)),
		)

		eventCh, errCh := w.client.Events(ctx, events.ListOptions{Filters: eventFilters})

		if err := w.reconcileServices(ctx); err != nil {
			logrus.WithError(err).Error("Docker Swarm resync failed")
		} else {
			backoff = time.Second
		}

		ticker := time.NewTicker(5 * time.Second)

	loop:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				if err := w.reconcileServices(ctx); err != nil {
					logrus.WithError(err).Error("Docker Swarm reconciliation failed")
				}
			case ev, ok := <-eventCh:
				if !ok {
					break loop
				}
				logrus.WithFields(logrus.Fields{"type": ev.Type, "action": ev.Action, "id": ev.Actor.ID}).Trace("Docker Swarm event")
				if err := w.reconcileServices(ctx); err != nil {
					logrus.WithError(err).Error("Docker Swarm reconciliation failed")
				}
			case err, ok := <-errCh:
				if !ok {
					break loop
				}
				if ctx.Err() != nil {
					ticker.Stop()
					return
				}
				logrus.WithError(err).Warn("Docker Swarm event stream error, reconnecting")
				break loop
			}
		}
		ticker.Stop()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (w *dockerSwarmWatcherImpl) listServices(ctx context.Context) ([]*routableSwarmService, error) {
	services, err := w.client.ServiceList(ctx, dockertypes.ServiceListOptions{})
	if err != nil {
		return nil, err
	}

	serverVersion, err := w.client.ServerVersion(ctx)
	if err != nil {
		return nil, err
	}

	networkListArgs := filters.NewArgs()
	// https://docs.docker.com/engine/api/v1.29/#tag/Network (Docker 17.06)
	if versions.GreaterThanOrEqualTo(serverVersion.APIVersion, "1.29") {
		networkListArgs.Add("scope", "swarm")
	} else {
		networkListArgs.Add("driver", "overlay")
	}

	networkList, err := w.client.NetworkList(ctx, network.ListOptions{Filters: networkListArgs})
	if err != nil {
		return nil, err
	}

	networkMap := make(map[string]*network.Inspect)
	for _, network := range networkList {
		networkToAdd := network
		networkMap[network.ID] = &networkToAdd
	}

	var result []*routableSwarmService
	for _, service := range services {
		if service.Spec.EndpointSpec == nil ||
			(service.Spec.EndpointSpec.Mode != swarmtypes.ResolutionModeVIP &&
				service.Spec.EndpointSpec.Mode != swarmtypes.ResolutionModeDNSRR) {
			continue
		}
		if service.Spec.EndpointSpec.Mode == swarmtypes.ResolutionModeVIP && len(service.Endpoint.VirtualIPs) == 0 {
			continue
		}

		data, ok := w.evaluateSwarmService(ctx, &service, networkMap)
		if !ok {
			continue
		}

		endpoint := ""
		if data.ip != "" {
			endpoint = fmt.Sprintf("%s:%d", data.ip, data.port)
		}

		for _, host := range data.hosts {
			result = append(result, &routableSwarmService{
				containerEndpoint:         endpoint,
				externalServiceName:       host,
				serviceID:                 data.serviceID,
				serviceName:               data.serviceName,
				networkID:                 data.networkID,
				autoScaleUp:               data.autoScaleUp,
				autoScaleDown:             data.autoScaleDown,
				autoScaleAsleepMOTD:       data.autoScaleAsleepMOTD,
				autoScaleLoadingMOTD:      data.autoScaleLoadingMOTD,
				autoScaleWaitTimeout:      data.autoScaleWaitTimeout,
				autoScaleFailedMOTD:       data.autoScaleFailedMOTD,
				autoScaleRestartDelayMOTD: data.autoScaleRestartDelayMOTD,
				countdownDeadline:         data.countdownDeadline,
				statusState:               data.statusState,
			})
		}
		if data.def != nil && *data.def {
			result = append(result, &routableSwarmService{
				containerEndpoint:         endpoint,
				externalServiceName:       "",
				serviceID:                 data.serviceID,
				serviceName:               data.serviceName,
				networkID:                 data.networkID,
				autoScaleUp:               data.autoScaleUp,
				autoScaleDown:             data.autoScaleDown,
				autoScaleAsleepMOTD:       data.autoScaleAsleepMOTD,
				autoScaleLoadingMOTD:      data.autoScaleLoadingMOTD,
				autoScaleWaitTimeout:      data.autoScaleWaitTimeout,
				autoScaleFailedMOTD:       data.autoScaleFailedMOTD,
				autoScaleRestartDelayMOTD: data.autoScaleRestartDelayMOTD,
				countdownDeadline:         data.countdownDeadline,
				statusState:               data.statusState,
			})
		}
	}

	return result, nil
}

func dockerCheckNetworkName(id string, name string, networkMap map[string]*network.Inspect, networkAliases map[string][]string) (bool, error) {
	// we allow to specify the id instead
	if id == name {
		return true, nil
	}
	if network := networkMap[id]; network != nil {
		if network.Name == name {
			return true, nil
		}
		aliases := networkAliases[id]
		for _, alias := range aliases {
			if alias == name {
				return true, nil
			}
		}
		return false, nil
	}

	return false, fmt.Errorf("network not found %s", id)
}

type parsedDockerServiceData struct {
	hosts                     []string
	port                      uint64
	def                       *bool
	network                   *string
	networkID                 string
	ip                        string
	serviceID                 string
	serviceName               string
	autoScaleUp               bool
	autoScaleDown             bool
	autoScaleAsleepMOTD       string
	autoScaleLoadingMOTD      string
	autoScaleWaitTimeout      time.Duration
	autoScaleFailedMOTD       string
	autoScaleRestartDelayMOTD string
	countdownDeadline         time.Time
	isDNSRR                   bool
	statusState               string
}

func (w *dockerSwarmWatcherImpl) evaluateSwarmService(ctx context.Context, service *swarm.Service, networkMap map[string]*network.Inspect) (data parsedDockerServiceData, ok bool) {
	data.autoScaleUp = w.config.autoScaleUp
	data.autoScaleDown = w.config.autoScaleDown
	data.autoScaleWaitTimeout = 60 * time.Second
	data.serviceID = service.ID
	data.serviceName = service.Spec.Name

	if !w.parseServiceLabels(service, &data) {
		return
	}

	// probably not minecraft related
	if len(data.hosts) == 0 {
		return
	}

	isVIP := service.Spec.EndpointSpec != nil && service.Spec.EndpointSpec.Mode == swarmtypes.ResolutionModeVIP
	isDNSRR := service.Spec.EndpointSpec != nil && service.Spec.EndpointSpec.Mode == swarmtypes.ResolutionModeDNSRR
	data.isDNSRR = isDNSRR

	if !isVIP && !isDNSRR {
		logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
			Warnf("ignoring service with unsupported endpoint resolution mode")
		return
	}

	if isVIP && len(service.Endpoint.VirtualIPs) == 0 {
		logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
			Warnf("ignoring service, no VirtualIPs found")
		return
	}

	if data.port == 0 {
		data.port = 25565
	}

	replicas := uint64(0)
	if service.Spec.Mode.Replicated != nil && service.Spec.Mode.Replicated.Replicas != nil {
		replicas = *service.Spec.Mode.Replicated.Replicas
	}

	data.networkID = resolveTargetNetwork(service, data.network, networkMap)

	var hasRunningTask bool
	var runningTaskIP string
	var hasReadyTask bool
	var readyTaskTimestamp time.Time
	var hasActiveTask bool
	var latestTask swarm.Task
	var delay time.Duration

	var tasks []swarm.Task
	var err error
	if replicas > 0 {
		tasks, err = w.client.TaskList(ctx, dockertypes.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("service", service.ID)),
		})
		if err == nil && len(tasks) > 0 {
			if service.Spec.TaskTemplate.RestartPolicy != nil {
				if service.Spec.TaskTemplate.RestartPolicy.Delay != nil {
					delay = *service.Spec.TaskTemplate.RestartPolicy.Delay
				}
			}

			for _, task := range tasks {
				state := task.Status.State
				desiredState := task.DesiredState

				// Track the most recently created task to inspect its DesiredState.
				if latestTask.CreatedAt.IsZero() || task.CreatedAt.After(latestTask.CreatedAt) {
					latestTask = task
				}

				// The task is considered fully Running only if both actual and desired states are running.
				// If DesiredState is Shutdown or Remove, it is stopping.
				if state == swarm.TaskStateRunning && desiredState == swarm.TaskStateRunning {
					hasRunningTask = true
					// Extract direct task IP to bypass VIP propagation lag when replicas == 1
					for _, attachment := range task.NetworksAttachments {
						matchesNetwork := data.networkID != "" && attachment.Network.ID == data.networkID
						isIngress := attachment.Network.Spec.Name == "ingress"

						if (matchesNetwork || (data.networkID == "" && !isIngress)) && len(attachment.Addresses) > 0 {
							parts := strings.Split(attachment.Addresses[0], "/")
							if ip := net.ParseIP(parts[0]); ip != nil {
								runningTaskIP = parts[0]
								break
							}
						}
					}
				}

				// Swarm task state 'ready' (actual or desired) marks a task held during a restart delay.
				// Find the latest ready task's status timestamp to measure the restart delay start.
				if state == swarm.TaskStateReady || desiredState == swarm.TaskStateReady {
					hasReadyTask = true
					if task.Status.Timestamp.After(readyTaskTimestamp) {
						readyTaskTimestamp = task.Status.Timestamp
					}
				}

				// Track active task states to see if Swarm is actively attempting to schedule/start a task.
				if state == swarm.TaskStateNew ||
					state == swarm.TaskStatePending ||
					state == swarm.TaskStateAssigned ||
					state == swarm.TaskStateAccepted ||
					state == swarm.TaskStatePreparing ||
					state == swarm.TaskStateReady ||
					state == swarm.TaskStateStarting ||
					state == swarm.TaskStateRunning ||
					desiredState == swarm.TaskStateReady ||
					desiredState == swarm.TaskStateRunning {
					hasActiveTask = true
				}
			}
		}
	}

	// State machine classification:
	// To prevent premature VIP/DNS resolution routing client connections away from the waker,
	// we keep data.ip = "" in all non-running states. This forces client connections to use
	// the waker, keeps the loading MOTD active, and avoids dialing starting/unreachable containers.

	if replicas == 0 {
		// 1. Sleeping State: Service is scaled to zero.
		data.statusState = "sleeping"
		data.ip = ""
		// data.autoScaleAsleepMOTD is already populated by parsing labels.
		data.countdownDeadline = time.Time{}
	} else if hasRunningTask {
		// 2. Running (Healthy) State: Service has a fully running task.
		data.statusState = "running"
		if replicas == 1 && runningTaskIP != "" {
			data.ip = runningTaskIP
		} else if isVIP {
			vipIndex := -1
			if data.networkID != "" {
				for i, vip := range service.Endpoint.VirtualIPs {
					if vip.NetworkID == data.networkID {
						vipIndex = i
						break
					}
				}
			}
			if vipIndex == -1 {
				if data.network != nil {
					networkAliases := map[string][]string{}
					for _, network := range service.Spec.TaskTemplate.Networks {
						networkAliases[network.Target] = network.Aliases
					}
					for i, vip := range service.Endpoint.VirtualIPs {
						if ok, err := dockerCheckNetworkName(vip.NetworkID, *data.network, networkMap, networkAliases); ok {
							vipIndex = i
							break
						} else if err != nil {
							logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
								Debugf("%v", err)
						}
					}
				} else {
					vipIndex = 0
				}
			}
			if vipIndex != -1 && vipIndex < len(service.Endpoint.VirtualIPs) {
				virtualIP := service.Endpoint.VirtualIPs[vipIndex]
				ip, _, _ := net.ParseCIDR(virtualIP.Addr)
				data.ip = ip.String()
				data.networkID = virtualIP.NetworkID
			} else {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service, unable to find match in VirtualIPs")
				return
			}
		} else if isDNSRR {
			data.ip = service.Spec.Name
		}
	} else {
		// Non-running states: keep data.ip empty to ensure the waker captures client connections
		data.ip = ""

		// 3. Restart Delay State: Swarm scheduler is delaying task retry, keeping it in the 'ready' state.
		if hasReadyTask && delay > 0 {
			data.statusState = "restart_delay"
			if data.autoScaleRestartDelayMOTD != "" {
				data.autoScaleAsleepMOTD = data.autoScaleRestartDelayMOTD
			} else if data.autoScaleFailedMOTD != "" {
				data.autoScaleAsleepMOTD = data.autoScaleFailedMOTD
			} else {
				data.autoScaleAsleepMOTD = "Server failed to start."
			}
			data.countdownDeadline = readyTaskTimestamp.Add(delay)
		} else if !hasActiveTask && latestTask.DesiredState == swarm.TaskStateShutdown && len(tasks) > 0 {
			// 4. Permanently Failed State: Swarm has commanded a shutdown on the failed task
			// and is no longer attempting to restart the service.
			data.statusState = "failed"
			if data.autoScaleFailedMOTD != "" {
				data.autoScaleAsleepMOTD = data.autoScaleFailedMOTD
			} else {
				data.autoScaleAsleepMOTD = "Server failed to start."
			}
			data.countdownDeadline = time.Time{}
		} else {
			// 5. Starting / Waking State: Swarm has scaled the service up and the task is starting up (transited past ready),
			// or it is the very first start. We display the loading MOTD on pings.
			data.statusState = "waking"
			if data.autoScaleLoadingMOTD != "" {
				data.autoScaleAsleepMOTD = data.autoScaleLoadingMOTD
			}
			data.countdownDeadline = time.Time{}
		}
	}

	ok = true
	return
}

func (w *dockerSwarmWatcherImpl) parseServiceLabels(service *swarm.Service, data *parsedDockerServiceData) bool {
	for key, value := range service.Spec.Labels {
		switch key {
		case DockerRouterLabelHost:
			if data.hosts != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelHost)
				return false
			}
			data.hosts = SplitExternalHosts(value)

		case DockerRouterLabelPort:
			if data.port != 0 {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelPort)
				return false
			}
			var err error
			data.port, err = strconv.ParseUint(value, 10, 32)
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid %s", DockerRouterLabelPort)
				return false
			}

		case DockerRouterLabelDefault:
			if data.def != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelDefault)
				return false
			}
			data.def = new(bool)

			lowerValue := strings.TrimSpace(strings.ToLower(value))
			*data.def = lowerValue != "" && lowerValue != "0" && lowerValue != "false" && lowerValue != "no"

		case DockerRouterLabelNetwork:
			if data.network != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					Warnf("ignoring service with duplicate %s", DockerRouterLabelNetwork)
				return false
			}
			data.network = new(string)
			*data.network = value

		case DockerRouterLabelAutoScaleUp:
			autoScaleUp, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid value for %s", DockerRouterLabelAutoScaleUp)
				return false
			}
			data.autoScaleUp = autoScaleUp

		case DockerRouterLabelAutoScaleDown:
			autoScaleDown, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid value for %s", DockerRouterLabelAutoScaleDown)
				return false
			}
			data.autoScaleDown = autoScaleDown

		case DockerRouterLabelAutoScaleAsleepMOTD:
			data.autoScaleAsleepMOTD = value

		case DockerRouterLabelAutoScaleLoadingMOTD:
			data.autoScaleLoadingMOTD = value

		case DockerRouterLabelAutoScaleWaitTimeout:
			dur, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil {
				logrus.WithFields(logrus.Fields{"serviceId": service.ID, "serviceName": service.Spec.Name}).
					WithError(err).
					Warnf("ignoring service with invalid value for %s", DockerRouterLabelAutoScaleWaitTimeout)
				return false
			}
			data.autoScaleWaitTimeout = dur

		case DockerRouterLabelAutoScaleFailedMOTD:
			data.autoScaleFailedMOTD = value

		case DockerRouterLabelAutoScaleRestartDelayMOTD:
			data.autoScaleRestartDelayMOTD = value
		}
	}
	return true
}

func resolveTargetNetwork(service *swarm.Service, labelNetwork *string, networkMap map[string]*network.Inspect) string {
	networkAliases := map[string][]string{}
	for _, network := range service.Spec.TaskTemplate.Networks {
		networkAliases[network.Target] = network.Aliases
	}

	if labelNetwork != nil {
		for _, netSpec := range service.Spec.TaskTemplate.Networks {
			if ok, _ := dockerCheckNetworkName(netSpec.Target, *labelNetwork, networkMap, networkAliases); ok {
				return netSpec.Target
			}
		}
	} else {
		// Default: Find the first non-ingress network in the task template
		for _, netSpec := range service.Spec.TaskTemplate.Networks {
			if network := networkMap[netSpec.Target]; network != nil {
				if network.Name != "ingress" {
					return netSpec.Target
				}
			}
		}
		// Fallback to first network if all are ingress or not found in networkMap
		if len(service.Spec.TaskTemplate.Networks) > 0 {
			return service.Spec.TaskTemplate.Networks[0].Target
		}
	}
	return ""
}
