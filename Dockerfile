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
# Não redeclarar USER: a base ":nonroot" já roda como UID numérica 65532 por padrão —
# um "USER nonroot" aqui sobrescreveria por um nome simbólico que o kubelet não consegue
# verificar contra runAsNonRoot (CreateContainerConfigError: "non-numeric user").
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/agent /agent
ENTRYPOINT ["/agent"]
