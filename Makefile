GO ?= go
PKG = $(shell $(GO) list ./... | grep -v -e '/drpchttp' -e '/internal/' -e '/cmd/')

.PHONY: all
all: build test vet lint

.PHONY: build
build:
	$(GO) build $(PKG)

.PHONY: test
test:
	$(GO) test $(PKG) -count=1

.PHONY: testrace
testrace:
	$(GO) test $(PKG) -race -count=1 -v

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: lint
lint:
	staticcheck $(PKG)
	golangci-lint run

.PHONY: gen-bazel
gen-bazel:
	@echo "Generating WORKSPACE"
	@echo 'workspace(name = "io_storj_drpc")' > WORKSPACE
	@echo 'Running gazelle...'
	$(GO) run github.com/bazelbuild/bazel-gazelle/cmd/gazelle@v0.40.0 \
		update --go_prefix=storj.io/drpc --exclude=examples --exclude=scripts --repo_root=.
	@echo 'You should now be able to build Cockroach using:'
	@echo '  ./dev build short -- --override_repository=io_storj_drpc=$(CURDIR)'

.PHONY: clean-bazel
clean-bazel:
	git clean -dxf WORKSPACE BUILD.bazel '**/BUILD.bazel'
