FROM scratch
COPY mc-router /
# create a temp directory for k8s library logging
COPY README.md /tmp/
ENTRYPOINT ["/mc-router"]