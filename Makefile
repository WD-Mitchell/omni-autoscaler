.PHONY: build run test docker-build docker-push

# Variables
IMAGE_REPO ?= ghcr.io/wd-mitchell/omni-autoscaler
IMAGE_TAG ?= latest

# Build the binary locally
build:
	go build -o bin/omni-autoscaler ./cmd/controller

# Run locally (requires KUBECONFIG and OMNI env vars)
run: build
	./bin/omni-autoscaler -config config.yaml -log-level debug

# Run tests
test:
	go test -v ./...

# Build Docker image
docker-build:
	docker build -t $(IMAGE_REPO):$(IMAGE_TAG) .

# Push Docker image
docker-push: docker-build
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

# Generate dependencies
deps:
	go mod tidy
	go mod download

# Lint
lint:
	golangci-lint run ./...
