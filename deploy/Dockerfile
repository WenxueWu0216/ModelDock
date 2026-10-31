# ModelDock control plane with the web console embedded. Static binary on
# distroless, non-root.
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${NODE_IMAGE} AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -tags embedweb -ldflags="-s -w" -o /out/modeldock ./cmd/modeldock

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/modeldock /usr/local/bin/modeldock
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/modeldock"]
CMD ["serve"]
