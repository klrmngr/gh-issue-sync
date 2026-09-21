.PHONY: build test test-db fmt vet run clean

build:
	go build -o gh-issue-sync .

fmt:
	gofmt -w .

vet:
	go vet ./...

# Unit tests only; database-backed tests skip without TEST_DATABASE_URL.
test: vet
	go test ./...

# Full suite against a throwaway Postgres.
test-db:
	@docker rm -f ghsync-test-pg >/dev/null 2>&1 || true
	@docker run -d --name ghsync-test-pg -e POSTGRES_PASSWORD=testpw \
		-e POSTGRES_USER=issue_sync -e POSTGRES_DB=issue_sync \
		-p 55432:5432 postgres:16-alpine >/dev/null
	@until docker exec ghsync-test-pg pg_isready -U issue_sync -d issue_sync >/dev/null 2>&1; do sleep 1; done
	-TEST_DATABASE_URL="postgres://issue_sync:testpw@localhost:55432/issue_sync?sslmode=disable" go test ./...
	@docker rm -f ghsync-test-pg >/dev/null

run: build
	./gh-issue-sync

clean:
	rm -f gh-issue-sync
