.PHONY: db todo test build gen gen-check

db:
	docker compose up -d

build:
	npm run build

todo: build
	go run ./examples/todo

# Regenerate the todo example's TypeScript schema from the Go server (needs the db).
gen:
	cd examples/todo && go run . -gen

# Fail if the generated schema is out of date (for CI).
gen-check:
	cd examples/todo && go run . -gen -check

test:
	go vet ./...
	go test ./...
	npm run typecheck
	npm test
