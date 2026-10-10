.PHONY: test integration vet fmt

fmt:
	gofmt -w .

vet:
	go vet ./...

test: vet
	go test -race -count=1 ./...

# Requires a PostgreSQL database, e.g.:
#   docker run --rm -e POSTGRES_HOST_AUTH_METHOD=trust -p 5432:5432 postgres:17
#   make integration PG_TEST_DSN="postgres://postgres@localhost:5432/postgres?sslmode=disable"
integration:
	PG_TEST_DSN="$(PG_TEST_DSN)" go test -race -count=1 -run Integration ./pg/...
