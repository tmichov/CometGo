.PHONY: db todo test test-logical chaos build gen gen-check bench bench-feed

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

# The Go suite on the logical feed (needs the db and COMETGO_TEST_DATABASE_URL).
test-logical:
	COMETGO_TEST_FEED=logical go test -count=1 .

# Fault-injection tests, as CI runs them (needs the db and COMETGO_TEST_DATABASE_URL).
chaos:
	COMETGO_TEST_PG_RESTART="docker compose restart postgres" go test -race -count=3 -run TestChaos .

# Load harness (needs the db): runs the base and slow scenarios and writes
# their reports to docs/bench/. See docs/bench/README.md.
bench:
	go run ./cmd/cometload -out docs/bench/base.md
	go run ./cmd/cometload -scenario slow -out docs/bench/slow.md
	go run ./cmd/cometload -scenario m3 -out docs/bench/m3-after.md

bench-feed:
	go run ./cmd/feedbench -writers 16 -out docs/bench/feedbench.md
	go run ./cmd/feedbench -writers 64 -out docs/bench/feedbench-64.md
	go run ./cmd/feedbench -writers 16 -payload 1800 -out docs/bench/feedbench-wide.md
