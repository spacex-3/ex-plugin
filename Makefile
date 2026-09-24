UNAME_S := $(shell uname -s)
ifeq ($(OS),Windows_NT)
PLUGIN_EXT := dll
else ifeq ($(UNAME_S),Darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

.PHONY: test build clean

test:
	go test ./...

build:
	mkdir -p dist
	go build -buildmode=c-shared -o dist/ex-plugin.$(PLUGIN_EXT) .
	rm -f dist/ex-plugin.h

clean:
	rm -rf dist
