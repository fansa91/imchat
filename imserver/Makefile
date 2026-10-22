VERSION ?= v0.5.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build linux-arm64 linux-amd64 all clean smoketest

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imserver ./cmd/server

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imserver-linux-arm64 ./cmd/server

linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imserver-linux-amd64 ./cmd/server

all: linux-arm64 linux-amd64

smoketest:
	go run ./cmd/smoketest -base http://127.0.0.1:8092

clean:
	rm -rf bin
