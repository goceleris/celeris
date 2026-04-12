# Makefile for the celeris Autobahn fuzzingclient harness.
#
# Usage:
#   make build          # build the celeris autobahn server binary
#   make run-std        # run the std-engine echo server on :9001
#   make run-epoll      # run the epoll engine on :9002 (linux only)
#   make run-iouring    # run the io_uring engine on :9003 (linux only)
#   make run-all        # run all three in the background
#   make stop           # stop background servers
#   make autobahn       # spin up servers + run the fuzzingclient via docker
#   make clean          # remove reports + binary
#
# Reports land in ./reports/clients/index.html after autobahn finishes.

GO        ?= go
BIN       ?= ./bin/autobahn-server
SRC       ?= ../../test/autobahn/server
PIDS_DIR  ?= ./.pids

.PHONY: build run-std run-epoll run-iouring run-all stop autobahn clean

build:
	mkdir -p $(dir $(BIN))
	cd ../.. && $(GO) build -o test/autobahn/$(BIN) ./test/autobahn/server

run-std: build
	$(BIN) -engine=std -addr=:9001

run-epoll: build
	$(BIN) -engine=epoll -addr=:9002

run-iouring: build
	$(BIN) -engine=iouring -addr=:9003

run-all: build
	mkdir -p $(PIDS_DIR)
	$(BIN) -engine=std     -addr=:9001 & echo $$! > $(PIDS_DIR)/std.pid
	@if [ "$$(uname -s)" = "Linux" ]; then \
		$(BIN) -engine=epoll   -addr=:9002 & echo $$! > $(PIDS_DIR)/epoll.pid; \
		$(BIN) -engine=iouring -addr=:9003 & echo $$! > $(PIDS_DIR)/iouring.pid; \
	fi
	@sleep 1

stop:
	-@if [ -d $(PIDS_DIR) ]; then \
		for f in $(PIDS_DIR)/*.pid; do \
			[ -f "$$f" ] && kill "$$(cat $$f)" 2>/dev/null || true; \
			rm -f "$$f"; \
		done; \
	fi

autobahn: run-all
	mkdir -p reports
	docker compose run --rm fuzzingclient
	$(MAKE) stop
	@echo "Reports: $$(pwd)/reports/clients/index.html"

clean: stop
	rm -rf reports bin $(PIDS_DIR)
