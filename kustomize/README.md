https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/

## Example

To use your own dev image, such as via [Github Packages](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry), create `kustomization.yml` and alter the overlay to choose `role` or `cluster-role`. This example assumes that a docker image pull secret has been created and named `ghrc-pull`, [see below](#creating-image-pull-secret).

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/itzg/mc-router/kustomize/overlays/role
  # OR
  # - https://github.com/itzg/mc-router/kustomize/overlays/cluster-role
images:
  - name: itzg/mc-router
    # replace your-user-org with your Github user/org and/or replace ghcr.io with your Docker registry
    newName: ghcr.io/your-user-org/mc-router-dev
patches:
  - target:
      name: mc-router
      kind: Deployment
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: _
      spec:
        template:
          spec:
            imagePullSecrets:
              - name: ghcr-pull
            containers:
              - name:  mc-router
                imagePullPolicy: Always
```

### Creating image pull secret

The following is an example of [creating an image pull secret](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_create/kubectl_create_secret_docker-registry/) named `ghrc-pull`. Be sure to replace `your-user-org` and the password will be a [personal access token](https://github.com/settings/tokens) with `read:packages` scope.

```shell
kubectl create secret docker-registry ghcr-pull --docker-server=ghcr.io --docker-username=your-user-org --docker-password=ghp_...
```

### Build and push your image

Be sure to replace `your-user-org`:

```shell
docker build -t ghcr.io/your-user-org/mc-router-dev
docker push ghcr.io/your-user-org/mc-router-dev
```

### Apply the kustomization

```shell
kubectl apply -k .
```

or if you want to preview what will be generated and applied:

```shell
kubectl kustomize
```