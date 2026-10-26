# syntax=docker/dockerfile:1
# Zero-dependency Go module, so no go.sum and no vendor step.
# GOTOOLCHAIN=local pins the toolchain to the image's own.
FROM golang:1.24-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY app/ .
# Build metadata is injected by the release workflow; a plain
# `docker build .` keeps the defaults.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.date=${BUILD_DATE}" \
      -o /inceptionlabs2api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /inceptionlabs2api /inceptionlabs2api
EXPOSE 3000
ENTRYPOINT ["/inceptionlabs2api"]
