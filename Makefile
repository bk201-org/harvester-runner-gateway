BINARY ?= hvst-runner-gw
CLIENT_BINARY ?= hvst-runner-gw-client
SMOKE_BINARY ?= hvst-runner-gw-smoke
OUTPUT_DIR ?= bin
IMAGE ?= harvester-runner-gateway:dev

.PHONY: build test vet docker-build clean

build:
	mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(BINARY) ./cmd/hvst-runner-gw
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(CLIENT_BINARY) ./cmd/hvst-runner-gw-client
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(SMOKE_BINARY) ./cmd/hvst-runner-gw-smoke

test:
	go test ./...

vet:
	go vet ./...

docker-build:
	docker build -t $(IMAGE) .

clean:
	rm -f $(OUTPUT_DIR)/$(BINARY) $(OUTPUT_DIR)/$(CLIENT_BINARY) $(OUTPUT_DIR)/$(SMOKE_BINARY)
