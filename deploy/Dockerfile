# ModelDock control plane. Static binary on distroless, non-root.
ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/modeldock ./cmd/modeldock

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/modeldock /usr/local/bin/modeldock
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/modeldock"]
CMD ["serve"]
