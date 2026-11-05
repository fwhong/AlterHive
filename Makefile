.PHONY: build run clean test vet split-init verify-split docker docker-up docker-down docker-build docker-logs

COMPOSE = docker compose --env-file deploy/manager-sensor/.env

build:
	go build -ldflags="-s -w" -o alterhive .

# standalone requires explicit ALTERHIVE_ADMIN_TOKEN (>=32 characters).
run: build
	./alterhive

clean:
	rm -f alterhive

test:
	go test -race ./...

vet:
	go vet ./...

split-init:
	python3 scripts/init-split.py

docker-build:
	scripts/build-local-image.sh alterhive:split

verify-split: docker-build
	scripts/verify-split.sh

docker-up: split-init docker-build
	$(COMPOSE) up -d

docker-down:
	$(COMPOSE) down

docker-logs:
	$(COMPOSE) logs -f

docker: docker-build
