TARGETS := darwin-arm64 darwin-x86_64 linux-arm64 linux-x86_64
WINDOWS_TARGETS := windows-arm64 windows-x86_64 windows-i686
SOURCES := $(filter-out %_test.go,$(wildcard *.go)) go.mod
go_arch = $(patsubst i686,386,$(patsubst x86_64,amd64,$(1)))

.PHONY: package test
package: $(addprefix bin/,$(addsuffix /agents,$(TARGETS))) $(addprefix bin/,$(addsuffix /agents.exe,$(WINDOWS_TARGETS)))
	@echo "Select this folder in CED: $(CURDIR)"
test: package
	go test -race ./...

bin/%/agents: $(SOURCES)
	mkdir -p $(@D)
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(call go_arch,$(word 2,$(subst -, ,$*))) go build -trimpath -buildvcs=false -o $@ .

bin/windows-%/agents.exe: $(SOURCES)
	mkdir -p $(@D)
	CGO_ENABLED=0 GOOS=windows GOARCH=$(call go_arch,$*) go build -trimpath -buildvcs=false -ldflags="-s -w" -o $@ .

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
