.PHONY: test test-decision test-scenarios vet run setup
GO := $(if $(wildcard .local-go/go/bin/go),$(CURDIR)/.local-go/go/bin/go,go)
export GOCACHE := $(CURDIR)/.local-go/cache
export GOMODCACHE := $(CURDIR)/.local-go/pkg/mod
export GOPATH := $(CURDIR)/.local-go/gopath
export GOTOOLCHAIN := local
test:
	$(GO) test -race ./...
test-decision:
	$(GO) test -race ./internal/decision
	$(GO) test -race ./internal/interpreter -run 'Test(DecisionStreams|Syllables|IncompleteCondition|PrefixCommit|Correction|InvalidLabel)' -v
test-scenarios:
	$(GO) test -race ./internal/scenario
	$(GO) test -race ./internal/interpreter -run TestAllReservationDialoguesThroughGo -v
vet:
	$(GO) vet ./...
run:
	./yoyaku serve
setup:
	./yoyaku setup
