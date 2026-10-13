.PHONY: db todo test chaos build gen gen-check bench

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

# Fault-injection tests, as CI runs them (needs the db and COMETGO_TEST_DATABASE_URL).
chaos:
	COMETGO_TEST_PG_RESTART="docker compose restart postgres" go test -race -count=3 -run TestChaos .

# Load harness (needs the db): runs the base and slow scenarios and writes
# their reports to docs/bench/. See docs/bench/README.md.
bench:
	go run ./cmd/cometload -out docs/bench/base.md
	go run ./cmd/cometload -scenario slow -out docs/bench/slow.md
	go run ./cmd/cometload -scenario m3 -out docs/bench/m3-before.md
