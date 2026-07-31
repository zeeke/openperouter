##@ QEMU E2E

QEMU_SSH_PORT ?= 2222
QEMU_K8S_PORT ?= 6443

.PHONY: qemu-image
qemu-image: ## Prepare the QEMU VM image with cloud-init
	hack/qemu/image/prepare-image.sh

.PHONY: qemu-setup
qemu-setup: ## Launch the QEMU VM, FRR TOR, and bridge (run qemu-image first)
	hack/qemu/vm/launch-vm.sh
	hack/qemu/vm/start-tor.sh

.PHONY: qemu-deploy
qemu-deploy: kustomize kubectl ## Deploy openperouter with grout inside the VM (run qemu-setup first)
	KUSTOMIZE=$(KUSTOMIZE) KUBECTL=$(KUBECTL) hack/qemu/vm/setup-vm.sh

.PHONY: qemu-e2etests
qemu-e2etests: ginkgo kubectl create-export-logs ## Run QEMU e2e tests (VM must be running)
	KUBECONFIG=$(CURDIR)/hack/qemu/vm/kubeconfig \
	  $(GINKGO) -v $(GINKGO_ARGS) --timeout=1h \
	  ./e2etests/suite -- \
	  --kubectl=$(KUBECTL) --groutmode --qemu-mode \
	  $(TEST_ARGS)

.PHONY: qemu-collect-logs
qemu-collect-logs: create-export-logs ## Collect QEMU VM and FRR TOR logs
	hack/qemu/vm/collect-logs.sh

.PHONY: qemu-clean
qemu-clean: ## Tear down the QEMU VM and FRR TOR
	hack/qemu/vm/stop-vm.sh
