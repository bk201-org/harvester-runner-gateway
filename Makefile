BINARY ?= hvst-runner-gw
CLIENT_BINARY ?= hvst-runner-gw-client
OUTPUT_BIN_DIR ?= bin
RELEASE_DIR ?= dist
IMAGE ?= harvester-runner-gateway:dev
DOCKER ?= docker
BUILD_FILE ?= Dockerfile.build

.PHONY: build cluster-release test test-cluster-action vet docker-build clean

build:
	$(DOCKER) build -f $(BUILD_FILE) --target binaries \
		--build-arg BINARY=$(BINARY) --build-arg CLIENT_BINARY=$(CLIENT_BINARY) \
		--output type=local,dest=$(OUTPUT_BIN_DIR) .

cluster-release:
	$(DOCKER) build -f $(BUILD_FILE) --target cluster-release \
		--output type=local,dest=$(RELEASE_DIR) .

test:
	$(DOCKER) build -f $(BUILD_FILE) --target test \
		--build-arg RUN_ID=$$(date +%s%N) .

test-cluster-action:
	$(DOCKER) build -f $(BUILD_FILE) --target test-cluster-action \
		--build-arg RUN_ID=$$(date +%s%N) .

vet:
	$(DOCKER) build -f $(BUILD_FILE) --target vet \
		--build-arg RUN_ID=$$(date +%s%N) .

image:
	$(DOCKER) build -t $(IMAGE) .


push:
	$(DOCKER) build -t $(IMAGE) . --push

clean:
	rm -f $(OUTPUT_BIN_DIR)/$(BINARY) $(OUTPUT_BIN_DIR)/$(CLIENT_BINARY)
