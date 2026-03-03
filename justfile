default: build

# Build both binaries
build: build-client build-server

build-client:
    go build -o meetty ./cmd/meetty

build-server:
    go build -o meetty-server ./cmd/meetty-server

# Run all tests
test:
    go test ./...

# Run all checks
check: fmt-check lint vet

# Check formatting
fmt-check:
    @test -z "$(gofmt -l .)" || (gofmt -l . && echo "Run 'just fmt' to fix" && exit 1)

# Run staticcheck
lint:
    staticcheck ./...

# Run go vet
vet:
    go vet ./...

# Format code
fmt:
    gofmt -w .

# Run go fix
fix:
    go fix ./...

# Clean build artifacts
clean:
    rm -f meetty meetty-server
