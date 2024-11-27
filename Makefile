default: 
	mkdir build
	go mod download

build-go:
	CGO_ENABLED=0 go build -o build/updater

build-docker:
	docker build -t localbuild/gluetun-qbt-port-updater --platform linux/amd64,linux/arm64 .