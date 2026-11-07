# Empty starts every database. Use make db-up DATABASES=postgres for just one.
DATABASES ?=
ALL_DATABASES := postgres,mysql,mariadb,sqlite,sqlserver
INTEGRATION := go test -count=1 ./tests/...
MODULES := . tools/antvet tools/antfmt tools/compare contrib/otel

.PHONY: bench test lint fmt db-up db-down test-integration test-pg test-mysql test-mariadb test-sqlite test-mssql

test:
	go test ./...

bench:
	go test -run '^$$' -bench . -benchtime 2s ./tests/benchmarks/

# fmt lays the code out, in two rounds because one tool's line breaks can give the
# other something to do. lint fails on anything fmt would change.
fmt:
	@for round in 1 2; do \
		go run -C tools/antfmt . $(CURDIR) || exit 1; \
		for module in $(MODULES); do (cd $$module && golangci-lint run --fix ./...) || exit 1; done; \
	done

# ANTVET is where lint builds the analyzer, out of the source tree.
ANTVET := $(shell go env GOCACHE)/antvet-lint

lint:
	go vet ./...
	go build -C tools/antvet -o $(ANTVET) ./cmd/antvet
	go vet -vettool=$(ANTVET) ./...
	staticcheck ./...
	go run -C tools/antfmt . -l $(CURDIR)
	@for module in $(MODULES); do (cd $$module && golangci-lint run ./...) || exit 1; done

db-up:
	docker compose up -d --wait $(DATABASES)

db-down:
	docker compose down -v

test-integration:
	ANT_TEST_DATABASES=$(ALL_DATABASES) $(INTEGRATION)

test-pg:
	ANT_TEST_DATABASES=postgres $(INTEGRATION)

test-mysql:
	ANT_TEST_DATABASES=mysql $(INTEGRATION)

test-mariadb:
	ANT_TEST_DATABASES=mariadb $(INTEGRATION)

test-sqlite:
	ANT_TEST_DATABASES=sqlite $(INTEGRATION)

test-mssql:
	ANT_TEST_DATABASES=sqlserver $(INTEGRATION)
