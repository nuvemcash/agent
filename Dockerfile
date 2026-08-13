# Build multi-arch: buildx passa TARGETOS/TARGETARCH; binário estático (CGO off).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=$VERSION" -o /out/agent ./cmd/agent

# distroless static: CA certs + tzdata + usuário não-root (UID 65532) embutidos.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/agent /agent
USER nonroot
ENTRYPOINT ["/agent"]
