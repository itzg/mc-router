
## Usage

```shell
kubectl apply -k examples/kustom-autoscale
```

## Notes

When using this example, make sure to change the "Patch in external server names" section of patches to apply your external server names if not wanting to use the `*.localhost` domain.

The example configures the pull policy of mc-router to `Always` for testing purposes.

### mc-router's NodePort

The kustomization initially exposes the mc-router service on a NodePort. To find the NodePort, such as when using Docker Desktop, run:

```shell
kubectl get svc mc-router -o jsonpath='{.spec.ports[0].nodePort}'
```

For testing with a consistent port of 25565, can also use port-forwarding such as

```shell
kubectl port-forward svc/mc-router 25565:25565
```
