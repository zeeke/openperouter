.PHONY: grout-deploy-helm
grout-deploy-helm: IMG_TAG=grout
grout-deploy-helm: HELM_ARGS=--set openperouter.grout.enabled=true
grout-deploy-helm: helm kind deploy-cluster grout-load-on-kind deploy-helm 

.PHONY: grout-docker-build
grout-docker-build:
	@if [ "$(CONTAINER_ENGINE)" = "podman" ]; then \
		sudo $(CONTAINER_ENGINE) build  -t ${IMG_REPO}/${IMG_NAME}:grout -f ./hack/grout/Dockerfile .; \
	else \
		$(CONTAINER_ENGINE) build -t ${IMG_REPO}/${IMG_NAME}:grout -f ./hack/grout/Dockerfile .; \
	fi	

.PHONY: grout-load-on-kind
grout-load-on-kind: kind
	$(MAKE) load-on-kind IMG=${IMG_REPO}/${IMG_NAME}:grout
