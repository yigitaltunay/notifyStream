.PHONY: test lint swagger

test:
	go test ./...

lint:
	golangci-lint run ./...

swagger:
	go run github.com/swaggo/swag/cmd/swag@latest init -g main.go -d ./cmd/api,./internal/api -o docs --parseDependency
