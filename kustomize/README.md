# mc-router Kustomize deployment

## Configure the deployment

Create a `kustomization.yaml` in your working directory and reference the overlay you want to customize, typically one of the [role scope overlays](#role-scope).

Apply your customized deployment from that directory with `kubectl apply -k .`. You can also use Kustomize patches to change deployment settings.

### Role scope overlays

The Kustomize overlays deploy mc-router with either a namespace-scoped `Role` or a `ClusterRole`. Choose the overlay that matches the permissions needed in your cluster:

- `https://github.com/itzg/mc-router/kustomize/overlays/role`
- `https://github.com/itzg/mc-router/kustomize/overlays/cluster-role`

> [!NOTE]
> The two role overlays will in turn bring in the `base` kustomization.

### Set mc-router environment variables

You can keep environment variables in a file and have Kustomize generate a ConfigMap from it. Create `mc-router.env` next to your `kustomization.yaml`, using `NAME=value` entries. mc-router options can be set using uppercase environment variable names; for example, `--api-binding` corresponds to `API_BINDING`:

```dotenv
DEBUG=true
API_BINDING=:8080
```

Add the file to `configMapGenerator` and reference the generated ConfigMap from the Deployment with `envFrom`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/itzg/mc-router/kustomize/overlays/role
configMapGenerator:
  - name: mc-router-env
    envs:
      - mc-router.env
patches:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: mc-router
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: mc-router
      spec:
        template:
          spec:
            containers:
              - name: mc-router
                envFrom:
                  - configMapRef:
                      name: mc-router-env
```

Kustomize generates the ConfigMap and updates the reference to include its generated name. Apply the deployment with `kubectl apply -k .`; when the env file changes, Kustomize generates a new ConfigMap and updates the Deployment. ConfigMaps are not for secrets—use a Kubernetes Secret for sensitive values.

## Example servers

The `examples` overlay deploys two sample Minecraft servers (`mc-latest` as the default route and `mc-paper` at `paper.your.domain`). It is standalone, so it can be combined with either the `role` or `cluster-role` overlay in your own `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/itzg/mc-router/kustomize/overlays/role
  - https://github.com/itzg/mc-router/kustomize/overlays/examples
```

See the [Kustomize documentation](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/) for more information about building and customizing deployments.

## Using development image

If the image is in a private registry, create an image pull secret and add its name to the Deployment's pod template using a patch:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mc-router
spec:
  template:
    spec:
      imagePullSecrets:
        - name: registry-pull
```

Create the secret in the same namespace as mc-router. For example:

```shell
kubectl create secret docker-registry registry-pull \
  --docker-server=your-registry.example.com \
  --docker-username=your-username \
  --docker-password=your-password
```

To use a custom image, use the `images` field in your `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/itzg/mc-router/kustomize/overlays/role
images:
  - name: itzg/mc-router
    newName: your-registry.example.com/your-org/mc-router
```