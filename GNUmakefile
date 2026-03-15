default: fmt lint install generate

# Pinned dependency overrides.
# go.podman.io/storage uses securejoin.OpenInRoot which was removed from the
# root package in v0.6.0+ (moved to pathrs-lite submodule). Pin to v0.4.1.
PINNED_DEPS = github.com/cyphar/filepath-securejoin@v0.4.1

deps:
	go get -u ./... $(PINNED_DEPS)
	go mod tidy

build:
	go build -tags containers_image_openpgp -v ./...

install: build
	go install -tags containers_image_openpgp -v ./...

lint:
	golangci-lint run --build-tags containers_image_openpgp

generate:
	cd tools; GOFLAGS="-tags=containers_image_openpgp" go generate ./...

fmt:
	gofmt -s -w -e .

test:
	go test -tags containers_image_openpgp -v -cover -timeout=120s -parallel=10 ./...

testacc:
	TF_ACC=1 go test -tags containers_image_openpgp -v -cover -timeout 120m ./...

.PHONY: fmt lint test testacc build install generate deps
