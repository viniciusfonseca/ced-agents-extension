TARGETS := darwin-arm64 darwin-x86_64 linux-arm64 linux-x86_64

.PHONY: package test
package: $(addprefix bin/,$(addsuffix /agents,$(TARGETS)))
	@echo "Select this folder in CED: $(CURDIR)"
test: package
	go test -race ./...

bin/%/agents: $(filter-out %_test.go,$(wildcard *.go)) go.mod
	mkdir -p $(@D)
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(subst x86_64,amd64,$(word 2,$(subst -, ,$*))) go build -trimpath -buildvcs=false -o $@ .

CED_SOURCE ?= ../ced
CED_BUILD ?= $(CED_SOURCE)/build-extensions.noindex
CED_LIBRARIES := ced_extensions ced_core ced_json ced_vterm

.PHONY: test-ced
test-ced: package
	@set -e; build_dir=$$(mktemp -d); \
	trap 'rm "$$build_dir/test-ced"; rmdir "$$build_dir"' EXIT; \
	$(CC) -std=c99 -D_DARWIN_C_SOURCE -D_POSIX_C_SOURCE=200809L -D_XOPEN_SOURCE=700 \
		-I$(CED_SOURCE)/src -I$(CED_SOURCE)/third_party/cjson \
		tests/ced_host.c $(addprefix $(CED_BUILD)/lib,$(addsuffix .a,$(CED_LIBRARIES))) \
		-pthread -lm -o "$$build_dir/test-ced"; \
	"$$build_dir/test-ced" "$(CURDIR)"
