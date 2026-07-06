# f1 runner build targets
VERSION := 0.3.1
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist test clean

build:
	go build -ldflags "$(LDFLAGS)" -o f1$(shell go env GOEXE) .

# Cross-compile everything `f1 server setup` might need to upload.
dist:
	GOOS=linux  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/f1-linux-amd64 .
	GOOS=linux  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/f1-linux-arm64 .
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/f1-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/f1-windows-amd64.exe .

test:
	go vet ./...
	go test ./...

clean:
	rm -rf dist f1 f1.exe
