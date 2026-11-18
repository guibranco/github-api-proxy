# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/github-api-proxy ./cmd/github-api-proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/github-api-proxy /github-api-proxy
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/github-api-proxy"]
