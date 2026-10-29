GO ?= go
GOLANGCI ?= golangci-lint
MIGRATE ?= migrate
DB_URL ?= postgres://wittychat:wittychat@localhost:5432/wittychat?sslmode=disable

.PHONY: build lint fmt test tidy migrate-up migrate-down dev-up dev-down

build: ## Build server + worker / 构建服务端与 worker
	$(GO) build ./server ./worker

lint: ## Lint all Go code / 全量 lint
	$(GOLANGCI) run ./...

fmt: ## Format code (gofumpt) / 格式化
	$(GOLANGCI) fmt ./...

test: ## Run tests / 跑测试
	$(GO) test ./...

tidy:
	$(GO) mod tidy

migrate-up: ## Apply DB migrations / 应用迁移
	$(MIGRATE) -path core/migrations -database "$(DB_URL)" up

migrate-down: ## Roll back one migration / 回退一步
	$(MIGRATE) -path core/migrations -database "$(DB_URL)" down 1

dev-up: ## Start dev dependencies (Postgres) / 启动开发依赖
	docker compose -f deploy/docker-compose.dev.yml up -d

dev-down:
	docker compose -f deploy/docker-compose.dev.yml down

bindings: ## Regenerate Wails TS bindings / 重新生成前端绑定
	cd desktop && wails3 generate bindings

frontend-build: bindings ## Build desktop frontend / 构建桌面前端
	cd desktop/frontend && corepack pnpm install --frozen-lockfile && corepack pnpm build
