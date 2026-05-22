BINARY := access-api-connect
DIST   := dist
LDFLAGS := -s -w
GOFLAGS := -trimpath -ldflags="$(LDFLAGS)"

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64

.PHONY: all build test vet tidy fmt clean release-snapshot $(PLATFORMS)

all: test build

build:
	go build $(GOFLAGS) -o $(BINARY) .

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

tidy:
	go mod tidy

fmt:
	gofmt -s -w .

clean:
	rm -rf $(DIST) $(BINARY)

# Cross-compile for every entry in $(PLATFORMS). CGO is always disabled so
# the builds are host-independent: a developer on either macOS or Linux can
# produce every artifact without installing a C cross-compiler. Linux/Windows
# binaries are fully static; macOS uses the pure-Go DNS resolver.
cross: $(PLATFORMS)

$(PLATFORMS):
	@os=$(word 1,$(subst /, ,$@)); arch=$(word 2,$(subst /, ,$@)); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out=$(DIST)/$(BINARY)-$$os-$$arch$$ext; \
	echo ">> building $$out"; \
	mkdir -p $(DIST); \
	GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build $(GOFLAGS) -o $$out .

# Local dry-run of the release pipeline (no upload, no tag required).
release-snapshot:
	goreleaser release --snapshot --clean
