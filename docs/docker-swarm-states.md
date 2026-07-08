# Docker Swarm Task States for Autoscaling

When building plugins or routers (like `mc-router`) that autoscale and monitor Docker Swarm services, interpreting task scheduling states is historically challenging due to sparse Docker documentation. 

This document defines how to map Swarm's **Actual Task Status State** (`task.Status.State`) and **Desired Task State** (`task.DesiredState`) to logical route status classifications.

---

## Task State Matrix

| State | Status State (`task.Status.State`) | Desired State (`task.DesiredState`) | Description |
| :--- | :--- | :--- | :--- |
| **Running** | `running` | `running` | The container is alive, healthy, and fully running. Connections can be routed directly. |
| **Restart Delay** | `ready` | `ready` | The service task crashed and Swarm is holding the next task retry until the restart delay timer expires. |
| **Starting / Waking** | `new`, `pending`, `assigned`, `preparing`, `ready`, `starting` | `running` | Swarm has scaled the replicas to `> 0` and is actively preparing or starting the container. |
| **Stopping** | `running` | `shutdown` or `remove` | Swarm has commanded the task to shut down or be removed, but the container is still cleaning up. |
| **Permanently Failed** | `failed`, `shutdown`, `rejected` | `shutdown` | Swarm has commanded a shutdown on the last failed container and is no longer attempting to schedule a new task. |

---

## State Classifications & Logic

### 1. Sleeping State
* **Condition**: `replicas == 0`
* **Interpretation**: The service is explicitly scaled down to zero replicas.
* **Router Action**: Keep the backend route empty (`""`) and display the configured asleep MOTD to prompt a wake-up on join.

### 2. Running (Healthy) State
* **Condition**: At least one task has `Status.State == running` and `DesiredState == running`.
* **Interpretation**: The server is fully online and ready for traffic.
* **Router Action**: Resolve the route to the service's Virtual IP (VIP) or DNS Round Robin (DNSRR) name.

### 3. Restart Delay State
* **Condition**: `replicas > 0` AND at least one task has `Status.State == ready` and `DesiredState == ready`.
* **Interpretation**: Swarm has scheduled a retry but is waiting for the configured restart policy delay to expire before spawning the container.
* **Router Action**: Keep the backend route empty (`""`) to route connection attempts to the waker, and display the restart delay countdown MOTD.

### 4. Permanently Failed State
* **Condition**: `replicas > 0` AND the most recently updated task in history has `DesiredState == shutdown` AND no active tasks exist.
* **Interpretation**: Swarm has exhausted its restart attempts and stopped trying.
* **Router Action**: Keep the backend route empty (`""`) and display the custom permanently failed MOTD.

### 5. Starting / Waking State
* **Condition**: `replicas > 0` AND the desired state of the task is `running` but the actual status is not yet `running` (and not delayed or failed).
* **Interpretation**: The container is actively being assigned, prepared, or started by Swarm.
* **Router Action**: Keep the backend route empty (`""`) to preserve the waker, and display the loading/waking MOTD.
