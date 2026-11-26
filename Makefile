export GOTOOLCHAIN := go1.24.6
.PHONY: check generate test test-integration test-e2e test-e2e-auth test-race

generate:
	cd backend && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0 generate && go tool gqlgen generate
check:
	cd backend && test -z "$$(gofmt -l $$(find . -name '*.go' -not -name 'generated.go'))" && go vet ./...
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; cp -R backend/internal/db "$$tmp/db"; cp -R backend/graphql/model "$$tmp/model"; cp backend/graphql/generated.go "$$tmp/generated.go"; cp backend/graphql/schema.resolvers.go "$$tmp/resolvers.go"; $(MAKE) generate && diff -ru "$$tmp/db" backend/internal/db && diff -ru "$$tmp/model" backend/graphql/model && diff -u "$$tmp/generated.go" backend/graphql/generated.go && diff -u "$$tmp/resolvers.go" backend/graphql/schema.resolvers.go
	cd apps/web && npm run lint && npm run typecheck && npm run build
test:
	cd backend && go test ./...
test-integration: DATABASE_URL ?= postgres://repoorbit:repoorbit@localhost:54329/repoorbit?sslmode=disable
test-integration:
	cd backend && DATABASE_URL='$(DATABASE_URL)' go test -tags=integration ./...
test-e2e-auth:
	./scripts/e2e-auth.sh
test-e2e:
	cd apps/web && npm run test:e2e
test-race:
	cd backend && go test -race ./...
