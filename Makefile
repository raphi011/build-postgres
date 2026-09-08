# Chapter targets run the packages of that chapter and every earlier one.

GO ?= go

CH01 = ./internal/page/...
CH02 = $(CH01) ./internal/tuple/... ./cmd/pgdb/...
CH03 = $(CH02) ./internal/smgr/...
CH04 = $(CH03) ./internal/bufmgr/...
CH05 = $(CH04) ./internal/heap/...
CH06 = $(CH05) ./internal/sql/lexer/...

.PHONY: build test vet regress test-ch01 test-ch02 test-ch03 test-ch04 test-ch05 test-ch06

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
test-ch02:
	$(GO) test -race $(CH02)
test-ch03:
	$(GO) test -race $(CH03)
test-ch04:
	$(GO) test -race $(CH04)
test-ch05:
	$(GO) test -race $(CH05)
test-ch06:
	$(GO) test -race $(CH06)
