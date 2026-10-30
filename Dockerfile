FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/markham-schedule ./cmd/markham-schedule
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/markham-schedule /markham-schedule
COPY --from=build --chown=65532:65532 /data /data
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/markham-schedule","-healthcheck"]
ENTRYPOINT ["/markham-schedule"]
