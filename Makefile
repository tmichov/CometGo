.PHONY: db todo test build

db:
	docker compose up -d

build:
	npm run build

todo: build
	go run ./examples/todo

test:
	go vet ./...
	go test ./...
	npm run typecheck
	npm test
