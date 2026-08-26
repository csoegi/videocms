.PHONY: dev dev-backend dev-frontend build build-backend build-frontend prod clean docker-build dckb dcktest publish bump-minor bump-patch

VERSION ?= $(shell cat VERSION.txt 2>/dev/null || echo "1.0.0")
LOCAL_IMAGE = videocms:local
DOCKERHUB_IMAGE = chrissoe/videocms
DEV_FRONTEND_HOST ?= 127.0.0.1
DEV_FRONTEND_PORT ?= 3000
DEV_BACKEND_HOST ?= 127.0.0.1
DEV_BACKEND_PORT ?= 3001
DEV_BACKEND_URL ?= http://$(DEV_BACKEND_HOST):$(DEV_BACKEND_PORT)
DEV_PUBLIC_ORIGIN ?= http://$(DEV_FRONTEND_HOST):$(DEV_FRONTEND_PORT)
DEV_MEDIA_PATH ?= /videos/qualitys

# --- PRODUCTION BUILDING TARGETS ---
dev:
	@DEV_FRONTEND_HOST="$(DEV_FRONTEND_HOST)" \
	DEV_FRONTEND_PORT="$(DEV_FRONTEND_PORT)" \
	DEV_BACKEND_HOST="$(DEV_BACKEND_HOST)" \
	DEV_BACKEND_PORT="$(DEV_BACKEND_PORT)" \
	DEV_BACKEND_URL="$(DEV_BACKEND_URL)" \
	DEV_PUBLIC_ORIGIN="$(DEV_PUBLIC_ORIGIN)" \
	DEV_MEDIA_PATH="$(DEV_MEDIA_PATH)" \
	./scripts/dev.sh

dev-backend:
	Host="$(DEV_BACKEND_HOST):$(DEV_BACKEND_PORT)" \
	FolderVideoQualitysPub="$(DEV_MEDIA_PATH)" \
	go tool air serve:main

dev-frontend:
	cd videocms-frontend && \
	NUXT_PUBLIC_API_URL="/api" \
	NUXT_PUBLIC_BASE_URL="$(DEV_PUBLIC_ORIGIN)" \
	NUXT_DEV_BACKEND_URL="$(DEV_BACKEND_URL)" \
	NUXT_DEV_MEDIA_PATH="$(DEV_MEDIA_PATH)" \
	bun run dev --host "$(DEV_FRONTEND_HOST)" --port "$(DEV_FRONTEND_PORT)"

# --- BUILD & RUN PRODUCTION LOCALLY ---
prod: build
	@echo "🚀 [Production] Launching Single-Server Standalone Application..."
	./videocms serve:main

# --- PRODUCTION BUILDING TARGETS ---

build: build-frontend build-backend
	@echo "📦 [Production] Unified Build Completed Successfully."

# Compile the Go API backend binary
build-backend:
	@echo "🐹 [Backend] Compiling Go Production Binary..."
	go build -o videocms main.go

# Generate the static frontend assets and sync to the backend
build-frontend:
	@echo "🏗️  [Frontend] Generating Static Files..."
	cd videocms-frontend && bun install --frozen-lockfile
	cd videocms-frontend && NUXT_PUBLIC_API_URL=/api bun run generate
	@echo "🧹 [Frontend] Syncing Assets..."
	mkdir -p public && rm -rf public/*
	cp -r videocms-frontend/.output/public/* public/

# --- DOCKER BUILD & DEPLOYMENT COMMANDS ---

# Build the local Docker image using buildx layout framework flags
docker-build:
	@echo "🐳 Building local architecture container matching $(LOCAL_IMAGE)..."
	docker buildx build \
		-f Dockerfile \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg CHANNEL=local \
		--build-arg DOCKER_IMAGE_TAG=$(LOCAL_IMAGE) \
		-t $(LOCAL_IMAGE) \
		--load .
	@echo "✅ Local Docker Image Built and Loaded."

# Run the latest locally built image wihtout pulling from registry. 
docker-run:
	@echo "🚀 Launching Local Docker Image ($(LOCAL_IMAGE))..."
	@echo "⚠️  Binding container as 'videocms-local' to prevent production collisions."
	-docker rm -f videocms-local 2>/dev/null || true
	docker run -d \
		--name videocms-local \
		-p 3000:3000 \
		-v $$(pwd)/database:/app/database \
		-v $$(pwd)/videos:/app/videos \
		$(LOCAL_IMAGE)
	@echo "✅ Container Running Local Image on http://localhost:3000"

# Build and push the Docker image to Docker Hub with both version tag and latest tag. 
# This is used in CI after a successful build to publish the new version of the application.
docker-publish:
	@echo "🐳 Building Production Multi-Platform Release Images for $(VERSION)..."
	docker buildx build \
		-f Dockerfile \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg CHANNEL=stable \
		--build-arg DOCKER_IMAGE_TAG=$(VERSION) \
		-t $(DOCKERHUB_IMAGE):$(VERSION) \
		-t $(DOCKERHUB_IMAGE):latest \
		--push .
	@echo "🚀 Multi-Platform Images Successfully Published to Docker Hub Repository."


# --- OTHER COMMANDS ---
clean:
	rm -rf public/* videocms
	@echo "🧹 Workspace Cleared."

dckb: docker-build

dcktest:
	docker run --rm -it -p 3000:3000 $(LOCAL_IMAGE)

publish:
	@echo "Publishing is handled by GitHub Actions from staging and master."
	@exit 1

# Increment the minor version number in VERSION.txt and update the version string in config/config.go accordingly.
bump-minor:
	@NEW_VERSION=$$(awk -F. '{print $$1"."$$2+1".0"}' VERSION.txt); \
	echo "Bumping minor to $$NEW_VERSION"; \
	printf "%s" "$$NEW_VERSION" > VERSION.txt; \
	sed -i "s/var VERSION string = \".*\"/var VERSION string = \"$$NEW_VERSION\"/" config/config.go

# Increment the patch version number in VERSION.txt and update the version string in config/config.go accordingly.
bump-patch:
	@NEW_VERSION=$$(awk -F. '{print $$1"."$$2"."$$3+1}' VERSION.txt); \
	echo "Bumping patch to $$NEW_VERSION"; \
	printf "%s" "$$NEW_VERSION" > VERSION.txt; \
	sed -i "s/var VERSION string = \".*\"/var VERSION string = \"$$NEW_VERSION\"/" config/config.go
