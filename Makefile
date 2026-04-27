.PHONY: build clean fmt lint test vulncheck generate docker-build docker-run docker-clean

WEB_DIR := cmd/firepit/web
D3_JS := $(WEB_DIR)/d3.v7.min.js
D3_FLAMEGRAPH_JS := $(WEB_DIR)/d3-flamegraph.min.js
D3_FLAMEGRAPH_CSS := $(WEB_DIR)/d3-flamegraph.css

generate: $(D3_JS) $(D3_FLAMEGRAPH_JS) $(D3_FLAMEGRAPH_CSS)

$(D3_JS):
	@mkdir -p $(WEB_DIR)
	curl -fsSL https://d3js.org/d3.v7.min.js -o $@

$(D3_FLAMEGRAPH_JS):
	@mkdir -p $(WEB_DIR)
	curl -fsSL https://cdn.jsdelivr.net/npm/d3-flame-graph@4.1.3/dist/d3-flamegraph.min.js -o $@

$(D3_FLAMEGRAPH_CSS):
	@mkdir -p $(WEB_DIR)
	curl -fsSL https://cdn.jsdelivr.net/npm/d3-flame-graph@4.1.3/dist/d3-flamegraph.css -o $@

build: $(D3_JS) $(D3_FLAMEGRAPH_JS) $(D3_FLAMEGRAPH_CSS)
	go build -o firepit ./cmd/firepit

clean:
	go clean
	rm -f firepit $(D3_JS) $(D3_FLAMEGRAPH_JS) $(D3_FLAMEGRAPH_CSS)

fmt:
	go tool gofumpt -w .

lint:
	go tool staticcheck -checks=all -show-ignored -tests  ./...

test:
	go clean -testcache
	go test ./...

vulncheck:
	go tool govulncheck ./...

docker-build:
	docker build -t firepit .

docker-run: docker-build
	docker run -it --rm \
		-p 4317:4317 \
		-p 4318:4318 \
		-p 8080:8080 \
		firepit

docker-clean:
	docker stop firepit 2>/dev/null || true
	docker rm firepit 2>/dev/null || true
