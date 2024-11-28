FROM --platform=$BUILDPLATFORM golang:1.23 AS build-image
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod updater.go ./
RUN go mod download
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o updater .

FROM --platform=$TARGETPLATFORM alpine:3 AS release-image
COPY --from=build-image /app/updater /updater
ENTRYPOINT ["/updater"]