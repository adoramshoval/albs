# Application Details
BINARY_NAME := albs
BUILD_DIR   := bin
MAIN_PATH   := main.go

# Go Parameters
#
# GO is discovered rather than assumed: the go on PATH may be older than the
# go directive in go.mod, which produces a wall of confusing GOROOT errors.
# Override explicitly with `make build GO=/path/to/go`.
GO          := $(shell scripts/preflight-go.sh 2>/dev/null)
GOFLAGS     := -v
LDFLAGS     := -w -s

.PHONY: all build check clean test tidy vet run help

all: tidy build

## check: Verifies a usable Go toolchain is available
check:
	@scripts/preflight-go.sh >/dev/null

## build: Compiles the binary to bin/
build: check
	@mkdir -p $(BUILD_DIR)
	@echo "Building $(BINARY_NAME) with $(GO)..."
	"$(GO)" build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)

## run: Builds and runs the binary (requires GIT_URL)
run: build
	@if [ -z "$(GIT_URL)" ]; then \
		echo "Error: GIT_URL argument is required. Example: make run GIT_URL=https://github.com/paketo-buildpacks/python"; \
		exit 1; \
	fi
	./$(BUILD_DIR)/$(BINARY_NAME) --git-url $(GIT_URL) $(FLAGS)

## test: Runs all unit and package tests
test: check
	@echo "Running tests..."
	"$(GO)" test -race ./...

## vet: Runs go vet across the module
vet: check
	"$(GO)" vet ./...

## tidy: Downloads missing modules and removes unused ones
tidy: check
	@echo "Tidying go.mod..."
	"$(GO)" mod tidy

## clean: Removes build artifacts and local cache
clean:
	@echo "Cleaning build artifacts and cache..."
	@rm -rf $(BUILD_DIR)
	@rm -rf .cache
	@rm -f *.cnb
	@rm -f *.versions.json

## help: Displays available Makefile target descriptions
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' |  sed -e 's/^/ /'
