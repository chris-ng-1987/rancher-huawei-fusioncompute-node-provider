BIN := docker-machine-driver-fusioncompute
PKG := github.com/chris-ng-1987/rancher-huawei-fusioncompute-node-provider/driver

.PHONY: build clean install tidy vet test fmt

build:
	go build -o $(BIN) .

tidy:
	go mod tidy

vet:
	go vet ./...

test:
	go test ./...

fmt:
	gofmt -w .

install: build
	install -m 0755 $(BIN) /usr/local/bin/$(BIN)

clean:
	rm -f $(BIN)
