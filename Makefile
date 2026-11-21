.PHONY: dev-api dev-debug dev-web web build run docker

# Run the Go API against ./data.
dev-api:
	YOUNITHM_CONFIG_DIR=./data go run ./cmd/younithm

# Same, plus the developer debug UI at http://localhost:2015/debug/ (no login,
# loopback only; see internal/debugui). Config dir ./data-debug keeps it apart
# from ./data.
dev-debug:
	YOUNITHM_DEBUG=1 YOUNITHM_DEBUG_UI=1 YOUNITHM_CONFIG_DIR=./data-debug go run ./cmd/younithm

# Frontend: Vite dev server on :5173, proxying the API to :2015 (run dev-api too).
dev-web:
	cd web && npm install && npm run dev

# Build the frontend into web/dist, which the Go binary embeds.
web:
	cd web && npm ci && npm run build

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/younithm ./cmd/younithm

run: build
	YOUNITHM_CONFIG_DIR=./data ./bin/younithm

docker:
	docker build -t younithm:dev .
