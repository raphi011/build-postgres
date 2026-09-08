# Chapter targets run the packages of that chapter and every earlier one.

GO ?= go

CH01 = ./internal/page/...

.PHONY: build test vet regress test-ch01

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

# SQL regression suite (chapter 11+).
regress:
	$(GO) test -race ./internal/regress/...

test-ch01:
	$(GO) test -race $(CH01)
