VERSION ?= 0.1.0
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

.PHONY: build test install dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o clauder .

test:
	go vet ./...
	go test ./...

install: build
	mkdir -p $(PREFIX)/bin
	install -m 0755 clauder $(PREFIX)/bin/clauder

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/clauder-$(VERSION)-$$os-$$arch; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out . || exit 1; \
	done

clean:
	rm -f clauder
	rm -rf dist
