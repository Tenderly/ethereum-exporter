IMAGE        := europe-west3-docker.pkg.dev/tenderly-project/tenderly/ethereum-exporter
VERSION_FILE := VERSION
PLATFORM     ?= linux/amd64

# Last pushed version (empty until the first push), and the next one to use.
CURRENT := $(shell cat $(VERSION_FILE) 2>/dev/null)
VERSION ?= $(if $(CURRENT),$(shell echo $(CURRENT) | awk -F. '{printf "%d.%d\n", $$1, $$2+1}'),1.0)

.PHONY: build push push-latest

build:
	docker buildx build \
		--platform $(PLATFORM) \
		--build-arg VERSION=$(VERSION) \
		--tag $(IMAGE):$(VERSION) \
		--load .

push: build
	docker push $(IMAGE):$(VERSION)
	@echo $(VERSION) > $(VERSION_FILE)
	@echo "pushed $(IMAGE):$(VERSION)"

push-latest:
	@test -n "$(CURRENT)" || { echo "nothing pushed yet, run 'make push' first"; exit 1; }
	docker tag $(IMAGE):$(CURRENT) $(IMAGE):latest
	docker push $(IMAGE):latest
