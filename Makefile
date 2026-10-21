# Convenience targets. The canonical checks are plain Go commands:
#   go build ./... && go vet ./... && go test ./...
APP := ./app

.PHONY: build vet test race cover docker run

build:
	cd $(APP) && go build -o app .

vet:
	cd $(APP) && go vet ./...

test:
	cd $(APP) && go test ./... -count=1

race:
	cd $(APP) && go test ./... -count=1 -race

cover:
	cd $(APP) && go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out | tail -1

docker:
	docker build -t inceptionlabs2api .

run:
	cd $(APP) && IL2API_ADDR=:3000 go run .
