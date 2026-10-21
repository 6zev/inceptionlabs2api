# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /inceptionlabs2api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /inceptionlabs2api /inceptionlabs2api
EXPOSE 3000
ENTRYPOINT ["/inceptionlabs2api"]
