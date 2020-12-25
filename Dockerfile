ARG TARGETPLATFORM
FROM --platform=$TARGETPLATFORM scratch
COPY mc-router /
ENTRYPOINT ["/mc-router"]
