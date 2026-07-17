BINARY=devops-control
BUILD_DIR=.
UI_DIR=ui

.PHONY: all build ui-build ui-deps clean run test

all: build

## Install UI dependencies
ui-deps:
	cd $(UI_DIR) && npm install

## Build the React frontend
ui-build:
	cd $(UI_DIR) && npm run build

## Build the Go binary with embedded UI
build: ui-build
	go build -o $(BINARY) ./cmd/devops-control/

## Build without rebuilding UI
build-fast:
	go build -o $(BINARY) ./cmd/devops-control/

## Run the server (requires DEPLOY_CONTROL_TOKEN)
run: build
	./$(BINARY)

test: ui-build
	go test ./...
	go vet ./...
	bash -n deploy/*.sh deploy/runner/*.sh scripts/*.sh

## Development: run UI dev server with Go backend
dev:
	@echo "Start Go backend in one terminal:"
	@echo "  DEPLOY_CONTROL_TOKEN=<token> go run ./cmd/devops-control/"
	@echo "Start UI dev server in another:"
	@echo "  cd ui && npm run dev"

## Clean build artifacts
clean:
	rm -f $(BINARY)
	rm -rf $(UI_DIR)/node_modules
	rm -rf $(UI_DIR)/dist
	rm -rf internal/api/ui/dist/
	rm -rf tmp/
