PNPM_VERSION := $(shell node -p "require('./package.json').packageManager.replace(/^pnpm@/, '')")
SONAR_SCANNER_IMAGE := threadpoint-sonar-scanner:12.1.0.3233_8.0.1-go1.26.8

.PHONY: help setup install ci-install format format-check format-prettier format-prettier-check format-toml format-toml-check format-shell format-shell-check format-python format-python-check test coverage sonar sonar-config sonar-image gofmt lint vet build docs-go docs-links ci

help:
	@printf 'threadpoint Go module targets:\n'
	@printf '  make setup  Enable package manager shims\n'
	@printf '  make install  Install pinned formatter dependencies\n'
	@printf '  make ci-install  Install formatter dependencies with the frozen lockfile\n'
	@printf '  make format  Apply source, documentation, and configuration formatting\n'
	@printf '  make format-check  Check source, documentation, and configuration formatting\n'
	@printf '  make test   Run module tests\n'
	@printf '  make coverage  Run tests and enforce 85%% module and package statement coverage\n'
	@printf '  make sonar  Run the Go SonarQube Cloud scan\n'
	@printf '  make gofmt  Check Go formatting\n'
	@printf '  make lint   Run golangci-lint checks\n'
	@printf '  make vet    Vet module packages\n'
	@printf '  make build  Build the threadpoint CLI into a temporary directory\n'
	@printf '  make docs-go  Serve local Go documentation\n'
	@printf '  make docs-links  Check Markdown links and local fragments, and test the checker\n'
	@printf '  make ci     Run module CI checks\n'

setup:
	corepack enable
	corepack prepare pnpm@$(PNPM_VERSION) --activate

install: setup
	pnpm install --ignore-workspace

ci-install: setup
	pnpm install --ignore-workspace --frozen-lockfile

format: format-prettier format-toml format-shell format-python

format-check: format-prettier-check format-toml-check format-shell-check format-python-check

format-prettier:
	pnpm --ignore-workspace run format:prettier

format-prettier-check:
	pnpm --ignore-workspace run format:prettier:check

format-toml:
	taplo fmt --config .taplo.toml

format-toml-check:
	taplo fmt --check --config .taplo.toml

format-shell:
	shfmt -w -ln=auto -i=2 -ci $$(git ls-files -- '*.sh')

format-shell-check:
	shfmt -d -ln=auto -i=2 -ci $$(git ls-files -- '*.sh')

format-python:
	ruff format $$(git ls-files -- '*.py')

format-python-check:
	ruff format --check $$(git ls-files -- '*.py')

test:
	GOWORK=off go test ./...

coverage:
	GOWORK=off go test -race -coverpkg=./... -coverprofile=coverage.out ./...
	sh scripts/check-coverage.sh coverage.out > coverage-summary.txt; status=$$?; cat coverage-summary.txt; exit $$status

sonar:
	SONAR_SCANNER_IMAGE="$(SONAR_SCANNER_IMAGE)" bash scripts/run-sonar.sh

sonar-image:
	docker build --platform linux/amd64 --tag "$(SONAR_SCANNER_IMAGE)" --file .sonar/Dockerfile .sonar

sonar-config:
	scripts/check-sonar-configuration.sh

gofmt:
	test -z "$$(gofmt -l .)"

lint:
	GOWORK=off golangci-lint run ./...

vet:
	GOWORK=off go vet ./...

build:
	out="$$(mktemp -d)"; \
	GOWORK=off go build -o "$$out/threadpoint" ./cmd/threadpoint; \
	test -s "$$out/threadpoint"; \
	printf 'built %s\n' "$$out/threadpoint"

docs-go:
	GOWORK=off go doc -http

docs-links:
	python3 scripts/test-docs-links.py
	sh scripts/check-docs-links.sh

ci: format-check sonar-config gofmt lint vet coverage build docs-links
