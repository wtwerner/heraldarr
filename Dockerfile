# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/heraldarr ./cmd/heraldarr

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heraldarr /heraldarr
VOLUME /config
EXPOSE 8790
HEALTHCHECK --interval=1m --timeout=10s CMD ["/heraldarr", "health"]
ENTRYPOINT ["/heraldarr"]
CMD ["serve"]
