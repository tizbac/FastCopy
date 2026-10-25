BINARIES = fastcopy fasttar
GO ?= go

all: $(BINARIES)

fastcopy: *.go internal/progress/*.go internal/fiemap/*.go
	$(GO) build -o fastcopy .

fasttar: cmd/fasttar/*.go internal/progress/*.go internal/fiemap/*.go
	$(GO) build -o fasttar ./cmd/fasttar

test: $(BINARIES)
	$(GO) test ./...
	./tests/parity.sh
	./tests/fuzz.sh
	./tests/tar_parity.sh

vet:
	$(GO) vet ./...

clean:
	rm -f $(BINARIES)
	rm -rf tests/out tests/fuzzout tests/out-tar

.PHONY: all test vet clean
