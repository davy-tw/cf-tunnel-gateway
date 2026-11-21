# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /manager ./cmd/manager

# The fake Cloudflare API for end-to-end tests (`docker build --target fakeapi`).
FROM build AS build-fakeapi
COPY test/fakeapi/ test/fakeapi/
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /fakeapi ./test/fakeapi

FROM gcr.io/distroless/static:nonroot AS fakeapi
COPY --from=build-fakeapi /fakeapi /fakeapi
USER 65532:65532
ENTRYPOINT ["/fakeapi"]

FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
