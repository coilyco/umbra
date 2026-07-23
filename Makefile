.PHONY: build test vet lint tidy fmt cover docs docs-cli docs-serve godoc-update release-artifacts pre-commit

build: ## Build all packages.
	go build ./...

test: ## Run the unit test suite.
	go test ./...

vet: ## go vet across the tree.
	go vet ./...

lint: ## Lint with golangci-lint.
	golangci-lint run ./...

tidy: ## go mod tidy.
	go mod tidy

fmt: ## Format Go source.
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

cover: ## Unit tests with a coverage profile.
	go test -coverprofile=coverage.out ./...

docs: ## Build the mkdocs site into ./site.
	mkdocs build --strict

docs-cli: ## Overlay the per-example CLI reference under site/cli/ via cli-web-docs.
	cd scripts/gen-webdocs && go run .

docs-serve: ## Serve mkdocs locally with live reload on 127.0.0.1:8000.
	mkdocs serve

godoc-update: ## Regenerate godoc-current.txt; commit the diff to land API changes.
	./scripts/check-godoc-current.sh --update

release-artifacts: ## Build the tagged kdl-specs binary matrix and SHA256SUMS.
	./scripts/build-kdl-specs-release.sh "$(VERSION)" "$(or $(DIST_DIR),dist)"

pre-commit: ## Run every repository hook.
	pre-commit run --all-files
