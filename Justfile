version := `git describe --tags --always --dirty 2>/dev/null || echo "dev"`
commit  := `git rev-parse --short HEAD 2>/dev/null || echo "unknown"`
date    := `date -u +%Y-%m-%dT%H:%M:%SZ`

ldflags  := "-X main.buildVersion=" + version + " -X main.buildCommit=" + commit + " -X main.buildDate=" + date
plugin   := "~/.gitlab-runner/plugins/fleeting-plugin-upcloud"

# Build the plugin binary into bin/ and ad-hoc sign it (required on macOS/Apple Silicon)
build:
    go build -ldflags "{{ldflags}}" -o bin/fleeting-plugin-upcloud .
    codesign --sign - bin/fleeting-plugin-upcloud

# Build, sign, and deploy directly to the runner plugins directory
deploy: build
    cp bin/fleeting-plugin-upcloud {{plugin}}

# Build for Linux (amd64 and arm64) into bin/linux-<arch>/
build-linux:
    GOOS=linux GOARCH=amd64 go build -ldflags "{{ldflags}}" -o bin/linux-amd64/fleeting-plugin-upcloud .
    GOOS=linux GOARCH=arm64 go build -ldflags "{{ldflags}}" -o bin/linux-arm64/fleeting-plugin-upcloud .

# Build for macOS (amd64 and arm64) into bin/darwin-<arch>/ and ad-hoc sign both
build-mac:
    GOOS=darwin GOARCH=amd64 go build -ldflags "{{ldflags}}" -o bin/darwin-amd64/fleeting-plugin-upcloud .
    GOOS=darwin GOARCH=arm64 go build -ldflags "{{ldflags}}" -o bin/darwin-arm64/fleeting-plugin-upcloud .
    codesign --sign - bin/darwin-amd64/fleeting-plugin-upcloud
    codesign --sign - bin/darwin-arm64/fleeting-plugin-upcloud

# Build for all common platforms into bin/<os>-<arch>/
build-all: build-linux build-mac

# Install the binary to $GOPATH/bin (makes it available on $PATH)
install:
    go install -ldflags "{{ldflags}}" .

# Run tests
test:
    go test ./...

# Run go vet
vet:
    go vet ./...

# Remove build artifacts
clean:
    rm -rf bin/
