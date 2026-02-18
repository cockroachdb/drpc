GO ?= go
COUNT ?= 1
BENCH ?= .
RUN ?= ^$$
TIMEOUT ?= 300s
PROFILES_DIR ?= /tmp/drpc-profiles
CPU_PROFILE ?= $(PROFILES_DIR)/cpu.pprof
MEM_PROFILE ?= $(PROFILES_DIR)/mem.pprof
BLOCK_PROFILE ?= $(PROFILES_DIR)/block.pprof
MUTEX_PROFILE ?= $(PROFILES_DIR)/mutex.pprof
TRACE_PROFILE ?= $(PROFILES_DIR)/trace.out
BLOCK_PROFILE_RATE ?= 1
MEM_PROFILE_RATE ?= 4096
MUTEX_PROFILE_FRACTION ?= 1

# Run benchmarks once
.PHONY: bench
bench:
	$(GO) test -bench=$(BENCH) -benchmem -count=$(COUNT) -timeout=$(TIMEOUT) .

# Run benchmarks with count=10 for use with benchstat.
# Usage: make bench-stat > before.txt  (then after changes) make bench-stat > after.txt
#        benchstat before.txt after.txt
.PHONY: bench-stat
bench-stat:
	$(GO) test -bench=$(BENCH) -benchmem -count=10 -timeout=$(TIMEOUT) .

.PHONY: bench-parallel-streams
bench-parallel-streams:
	$(GO) test -run=$(RUN) -bench=BenchmarkParallelActiveStreams -benchmem -count=$(COUNT) -timeout=$(TIMEOUT) .

# Collect benchmark profiles in one run.
# Example:
# make bench-profile BENCH='BenchmarkParallelActiveStreams/size=1KB' COUNT=1
.PHONY: bench-profile
bench-profile:
	mkdir -p $(PROFILES_DIR)
	$(GO) test -run=$(RUN) -bench=$(BENCH) -benchmem -count=$(COUNT) -timeout=$(TIMEOUT) \
		-cpuprofile=$(CPU_PROFILE) \
		-memprofile=$(MEM_PROFILE) -memprofilerate=$(MEM_PROFILE_RATE) \
		-blockprofile=$(BLOCK_PROFILE) -blockprofilerate=$(BLOCK_PROFILE_RATE) \
		-mutexprofile=$(MUTEX_PROFILE) -mutexprofilefraction=$(MUTEX_PROFILE_FRACTION) \
		-trace=$(TRACE_PROFILE) .

.PHONY: profile-top-cpu
profile-top-cpu:
	$(GO) tool pprof -top $(CPU_PROFILE)

.PHONY: profile-top-alloc-space
profile-top-alloc-space:
	$(GO) tool pprof -top -sample_index=alloc_space $(MEM_PROFILE)

.PHONY: profile-top-inuse-space
profile-top-inuse-space:
	$(GO) tool pprof -top -sample_index=inuse_space $(MEM_PROFILE)

.PHONY: profile-top-block
profile-top-block:
	$(GO) tool pprof -top $(BLOCK_PROFILE)

.PHONY: profile-top-mutex
profile-top-mutex:
	$(GO) tool pprof -top $(MUTEX_PROFILE)

.PHONY: gen-proto
gen-proto:
	$(MAKE) -C ../.. install-protoc-plugin
	$(GO) generate .
