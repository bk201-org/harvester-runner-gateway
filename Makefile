BINARY ?= harvester-runner-gateway
CLIENT_BINARY ?= harvester-runner-gateway-client
OUTPUT_DIR ?= bin
IMAGE ?= harvester-runner-gateway:dev

.PHONY: build test vet docker-build clean

build:
	mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(BINARY) ./cmd/harvester-runner-gateway
	CGO_ENABLED=0 go build -trimpath -o $(OUTPUT_DIR)/$(CLIENT_BINARY) ./cmd/harvester-runner-gateway-client

test:
	go test ./...

vet:
	go vet ./...

docker-build:
	docker build -t $(IMAGE) .

clean:
	rm -f $(OUTPUT_DIR)/$(BINARY) $(OUTPUT_DIR)/$(CLIENT_BINARY)
