BINARY ?= hvst-runner-gw
CLIENT_BINARY ?= hvst-runner-gw-client
OUTPUT_DIR ?= bin
RELEASE_DIR ?= dist
IMAGE ?= harvester-runner-gateway:dev

.PHONY: build cluster-release test test-cluster-action vet docker-build clean

build:
	mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(BINARY) ./cmd/hvst-runner-gw
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(CLIENT_BINARY) ./cmd/hvst-runner-gw-client

cluster-release:
	mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(RELEASE_DIR)/hvst-runner-gw-cluster-linux-amd64 ./cmd/hvst-runner-gw-cluster
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(RELEASE_DIR)/hvst-runner-gw-cluster-linux-arm64 ./cmd/hvst-runner-gw-cluster
	cd $(RELEASE_DIR) && sha256sum hvst-runner-gw-cluster-linux-amd64 hvst-runner-gw-cluster-linux-arm64 > SHA256SUMS

test:
	go test ./...

test-cluster-action:
	go test ./internal/clusteraction -count=1
	node --test actions/create-ci-cluster/launcher.test.js
	bash -n scripts/cluster-smoke.sh

vet:
	go vet ./...

docker-build:
	docker build -t $(IMAGE) .

clean:
	rm -f $(OUTPUT_DIR)/$(BINARY) $(OUTPUT_DIR)/$(CLIENT_BINARY)
