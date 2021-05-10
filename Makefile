generate:
	go generate ./...
lint:
	golangci-lint run
test:
	go test -race -cover ./...
