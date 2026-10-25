BINARY ?= fastcopy
GO ?= go

all: $(BINARY)

$(BINARY): main.go flags.go copy.go progress.go fiemap.go util.go
	$(GO) build -o $(BINARY) .

test: $(BINARY)
	$(GO) test .
	./tests/parity.sh
	./tests/fuzz.sh

vet:
	$(GO) vet .

clean:
	rm -f $(BINARY)
	rm -rf tests/out

.PHONY: all test vet clean
