FROM scratch
COPY mc-router /
ENTRYPOINT ["/mc-router"]