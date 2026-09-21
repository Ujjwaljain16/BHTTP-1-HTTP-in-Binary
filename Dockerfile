# A ready-to-test server: bserve serving the conformance files on port 9000.
#
#   docker build -t bhttp .
#   docker run --rm -p 9000:9000 bhttp
#
# bchaos and bcurl are in the image too; override the entrypoint to use them:
#   docker run --rm --entrypoint /bchaos bhttp -target host.docker.internal:9000 -listen 0.0.0.0:9100 -chunk 1
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/bserve ./cmd/bchaos ./cmd/bcurl

FROM scratch
COPY --from=build /out/ /
COPY conformance/www /www
EXPOSE 9000
ENTRYPOINT ["/bserve"]
CMD ["/www", "9000"]
