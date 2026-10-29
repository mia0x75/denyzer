.PHONY: all clean serve_docs generate generate-check dashboard build test race fuzz e2e-test e2e-test-coverage lint run fmt docker-build help check-tools check-go check-docker check-bun
.DEFAULT_GOAL:=help

VERSION?=$(shell git describe --always --tags)
BUILD_TIME?=$(shell date --iso-8601=seconds)
DOC_PATH?="main"
DOCKER_IMAGE_NAME=mia0x75/denyzer

BINARY_NAME:=denyzer
BIN_OUT_DIR?=bin

GOARCH?=$(shell go env GOARCH)
GOARM?=$(shell go env GOARM)

# The SQLite driver is native, so every build needs CGO. Cross builds need an
# explicit CGO_ENABLED=1 because Go disables cgo when GOARCH differs.
CGO_ENABLED?=1
export CGO_ENABLED

GO_BUILD_FLAGS?=-v -trimpath
# Extra linker flags, e.g. `-linkmode external -extldflags -static` for the
# fully static container build. Kept as a separate variable so the default
# local build stays dynamic and fast.
EXTRA_LD_FLAGS?=
GO_BUILD_LD_FLAGS:=\
	-w \
	-s \
	${EXTRA_LD_FLAGS} \
	-X github.com/mia0x75/denyzer/util.Version=${VERSION} \
	-X github.com/mia0x75/denyzer/util.BuildTime=$(shell date -d "${BUILD_TIME}" '+%Y%m%d-%H%M%S') \
	-X github.com/mia0x75/denyzer/util.Architecture=${GOARCH}${GOARM}

GO_BUILD_OUTPUT:=$(BIN_OUT_DIR)/$(BINARY_NAME)$(BINARY_SUFFIX)

# Denyzer frontend dashboard (Next.js static export), embedded into the Go
# binary via go:embed from web/dashboard. Next exports into frontend/out
# (Turbopack rejects a distDir outside the project), then the exported files
# are copied into web/dashboard; the directory is gitignored except for
# .gitkeep so a clean clone can still compile the go:embed without the
# frontend toolchain.
FRONTEND_DIR?=frontend
DASHBOARD_OUT_DIR?=web/dashboard

# define version of golangci-lint here. If defined in tools.go, go mod perfoms automatically downgrade to older version which doesn't work with golang >=1.18
GOLANG_LINT_VERSION=v2.13.2

GINKGO_PROCS?=

# Fuzzing. Fuzz target seed corpora run as ordinary tests on every `make test`;
# this is the opt-in discovery mode that actively generates new inputs. `go test
# -fuzz` only fuzzes one target in one package per invocation, so `make fuzz`
# loops over every Fuzz* target in FUZZ_PKGS, time-boxing each at FUZZ_TIME.
FUZZ_TIME?=30s
FUZZ_PKGS?=./config ./util ./lists/parsers

# Parallelism for e2e tests. e2e specs are dominated by container startup and
# health-check waits rather than CPU, so oversubscribing beyond the core count
# improves wall-clock time. Defaults to -p (one process per core) for local
# runs; CI overrides this to oversubscribe the runner.
GINKGO_E2E_PROCS?=-p

export PATH=$(shell go env GOPATH)/bin:$(shell echo $$PATH)

# Tool check functions
define check_command
	@which $(1) > /dev/null 2>&1 || { echo "Error: $(1) is required but not installed." $(2); exit 1; }
endef

define check_go_tool
	@go list -f "{{.ImportPath}}" $(1) > /dev/null 2>&1 || { echo "Error: $(1) is required but not installed. Run: go install $(1)@latest"; exit 1; }
endef

check-go:
	$(call check_command,go,"Please install Go from https://golang.org/doc/install")

# The SQLite driver is native, so a C compiler is mandatory.
check-cc:
	$(call check_command,$(firstword $(CC)),"Please install a C compiler (gcc or clang); the SQLite driver needs CGO")

check-docker:
	$(call check_command,docker,"Please install Docker from https://docs.docker.com/get-docker/")
	@docker buildx version > /dev/null 2>&1 || { echo "Error: docker buildx is required but not installed. See https://docs.docker.com/buildx/working-with-buildx/"; exit 1; }

all: build test lint ## Build binary (with tests)

clean: ## cleans output directory
	rm -rf $(BIN_OUT_DIR)/*

serve_docs: check-docker ## serves online docs using Docker
	docker run --rm -p 8000:8000 -v $(PWD):/docs squidfunk/mkdocs-material:latest

generate: check-go ## Go generate
ifdef GO_SKIP_GENERATE
	$(info skipping go generate)
else
	env -u GOOS -u GOARCH go tool mockery
	env -u GOOS -u GOARCH go generate ./...
endif

generate-check: check-go ## Verify generated files (mocks, enums, config schema) are up to date
	env -u GOOS -u GOARCH go tool mockery
	env -u GOOS -u GOARCH go generate ./...
	@git diff --exit-code || { echo "generated files are out of date; run 'make generate' and commit the result"; exit 1; }

dashboard: $(if $(GO_SKIP_DASHBOARD),,check-bun) ## Build the Denyzer frontend dashboard (static export) into web/dashboard so it is embedded in the Go binary
ifdef GO_SKIP_DASHBOARD
	$(info skipping dashboard build (GO_SKIP_DASHBOARD is set))
else
	@test -d "$(FRONTEND_DIR)" || { echo "error: $(FRONTEND_DIR) does not exist"; exit 1; }
	@echo "Installing frontend dependencies..."
	cd "$(FRONTEND_DIR)" && bun install
	@echo "Building static export..."
	cd "$(FRONTEND_DIR)" && bun run build
	@echo "Copying export into $(DASHBOARD_OUT_DIR)..."
	@# Replace generated files but keep the tracked placeholder so the go:embed
	@# pattern keeps compiling on clean clones and git stays clean.
	find "$(DASHBOARD_OUT_DIR)" -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
	cp -R "$(FRONTEND_DIR)/out/." "$(DASHBOARD_OUT_DIR)/"
	touch "$(DASHBOARD_OUT_DIR)/.gitkeep"
	@echo "Done. Dashboard embedded at $(DASHBOARD_OUT_DIR)."
endif

check-bun:
	$(call check_command,bun,"Please install Bun from https://bun.sh/docs/installation")

build: check-go check-cc generate dashboard ## Build binary
	@mkdir -p $(BIN_OUT_DIR)
	go build $(GO_BUILD_FLAGS) -ldflags="$(GO_BUILD_LD_FLAGS)" -o $(GO_BUILD_OUTPUT)
ifdef BIN_USER
	$(info setting owner of $(GO_BUILD_OUTPUT) to $(BIN_USER))
	chown $(BIN_USER) $(GO_BUILD_OUTPUT)
endif
ifdef BIN_AUTOCAB
	$(info setting cap_net_bind_service (permitted) on $(GO_BUILD_OUTPUT))
	setcap 'cap_net_bind_service=+p' $(GO_BUILD_OUTPUT)
endif

test: check-go ## run tests
	go tool ginkgo --label-filter="!e2e" --coverprofile=coverage.txt --covermode=atomic --cover -r ${GINKGO_PROCS}
	go tool cover -html coverage.txt -o coverage.html

fuzz: check-go ## run each fuzz target for FUZZ_TIME (default 30s); e.g. make fuzz FUZZ_TIME=2m
	@set -e; \
	for pkg in $(FUZZ_PKGS); do \
		for target in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
			echo "==> fuzzing $$target in $$pkg for $(FUZZ_TIME)"; \
			go test -run '^$$' -fuzz "^$$target$$" -fuzztime $(FUZZ_TIME) $$pkg; \
		done; \
	done

e2e-test: check-go check-docker ## run e2e tests
	docker buildx build \
		--build-arg VERSION=denyzer-e2e \
		--build-arg BUILD_TIME=${BUILD_TIME} \
		--build-arg GOPROXY \
		--network=host \
		-o type=docker \
		-t denyzer-e2e \
		.
	go tool ginkgo ${GINKGO_E2E_PROCS} --label-filter="e2e" --timeout 15m --flake-attempts 1 e2e

e2e-test-coverage: check-go check-docker ## run e2e tests with code coverage
	@echo "Building coverage-instrumented Docker image..."
	docker buildx build \
		--build-arg VERSION=denyzer-e2e-coverage \
		--build-arg BUILD_TIME=${BUILD_TIME} \
		--build-arg GOPROXY \
		--build-arg OPTS="-cover" \
		--network=host \
		-o type=docker \
		-t denyzer-e2e-coverage \
		.
	@echo "Running e2e tests with coverage collection..."
	@mkdir -p coverage/e2e
	@rm -rf coverage/e2e/*
	@chmod 777 coverage/e2e
	DENYZER_IMAGE=denyzer-e2e-coverage GOCOVERDIR=$(PWD)/coverage/e2e \
		go tool ginkgo ${GINKGO_E2E_PROCS} --label-filter="e2e" --timeout 15m --flake-attempts 1 e2e
	@echo "Converting coverage data..."
	go tool covdata textfmt -i=./coverage/e2e -o=coverage/e2e-coverage.out
	@echo ""
	@echo "Coverage report generated!"
	@echo "===================="
	@go tool cover -func=coverage/e2e-coverage.out | awk '\
		BEGIN { print "\nCoverage by package:" } \
		/^total:/ { total = $$3; next } \
		/:/ { \
			split($$1, parts, ":"); \
			file = parts[1]; \
			n = split(file, path, "/"); \
			if (n > 1) { \
				pkg = path[1]; \
				for (i = 2; i < n; i++) pkg = pkg "/" path[i]; \
			} else { \
				pkg = "."; \
			} \
			gsub(/%/, "", $$3); \
			sum[pkg] += $$3; \
			count[pkg]++; \
		} \
		END { \
			for (pkg in sum) { \
				printf "%-60s %6.1f%%\n", pkg, sum[pkg]/count[pkg]; \
			} \
			print "------------------------------------------------------------"; \
			printf "%-60s %s\n", "TOTAL", total; \
		}' | sort
	@echo ""
	@echo "View full coverage report:"
	@echo "  - HTML: go tool cover -html=coverage/e2e-coverage.out"
	@echo "  - Text: go tool cover -func=coverage/e2e-coverage.out"

race: check-go ## run tests with race detector
	go tool ginkgo --label-filter="!e2e" --race -r ${GINKGO_PROCS}

lint: check-go fmt ## run golangcli-lint checks
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANG_LINT_VERSION) run --timeout 5m

run: build ## Build and run binary
	./$(BIN_OUT_DIR)/$(BINARY_NAME)

fmt: check-go ## gofmt and goimports all go files
	find . -type d \( -name node_modules -o -name vendor -o -name .git \) -prune -o -name '*.go' -print0 | xargs -0 -r go tool gofumpt -l -w -extra
	find . -type d \( -name node_modules -o -name vendor -o -name .git \) -prune -o -name '*.go' -print0 | xargs -0 -r go tool goimports -w

docker-build: check-docker generate ## Build docker image
	docker buildx build \
		--build-arg VERSION=${VERSION} \
		--build-arg BUILD_TIME=${BUILD_TIME} \
		--build-arg GOPROXY \
		--build-arg DOC_PATH=${DOC_PATH} \
		--network=host \
		-o type=docker \
		-t ${DOCKER_IMAGE_NAME} \
		.

check-tools: check-go check-docker ## Check if all required tools are installed

help:  ## Shows help
	@grep -E '^[0-9a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
