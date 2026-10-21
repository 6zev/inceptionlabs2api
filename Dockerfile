# syntax=docker/dockerfile:1
# Mirrors Sliverkiss/simple-chat's Dockerfile (the proven belmo-deployable shape):
# pinned golang:1.23-alpine, GOTOOLCHAIN=local so Go never tries to fetch a
# newer toolchain inside the builder, zero-dependency module needs no go.sum.
FROM golang:1.23-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY app/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /inceptionlabs2api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /inceptionlabs2api /inceptionlabs2api
EXPOSE 3000
ENTRYPOINT ["/inceptionlabs2api"]
