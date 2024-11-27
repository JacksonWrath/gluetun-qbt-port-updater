DOCKER_IMAGE_TAG=localbuild/gluetun-qbt-port-updater

default: 
	mkdir -p build
	go mod download

build-go:
	CGO_ENABLED=0 go build -o build/updater

build-docker:
	docker build -t $(DOCKER_IMAGE_TAG) --platform linux/amd64,linux/arm64 .

build-docker-arm64:
	docker build -t $(DOCKER_IMAGE_TAG) --platform linux/arm64 .

build-docker-amd64:
	docker build -t $(DOCKER_IMAGE_TAG) --platform linux/amd64 .