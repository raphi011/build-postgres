# Chapter targets run the packages of that chapter and every earlier one.
# Package lists are filled in as chapters land.

GO ?= go

.PHONY: build test vet regress

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

# SQL regression suite (chapter 11+).
regress:
	$(GO) test -race ./internal/regress/...
