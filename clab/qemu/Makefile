##@ QEMU E2E

QEMU_SSH_PORT ?= 2222
QEMU_K8S_PORT ?= 6443
QEMU_DIR := clab/qemu

.PHONY: qemu-image
qemu-image: ## Prepare the QEMU VM image with cloud-init
	$(QEMU_DIR)/image/prepare.sh

.PHONY: qemu-launch
qemu-launch: ## Launch the QEMU VM (run qemu-image and clab-qemu first)
	$(QEMU_DIR)/vm/launch.sh

.PHONY: qemu-setup
qemu-setup: kubectl ## Bootstrap k3s cluster inside the VM (run qemu-launch first)
	KUBECONFIG_PATH=$(KUBECONFIG_PATH) KUBECTL=$(KUBECTL) $(QEMU_DIR)/vm/setup.sh

.PHONY: qemu-load-image
qemu-load-image: ## Load the container image into the QEMU VM's k3s
	$(QEMU_DIR)/vm/load-image.sh $(IMG)

.PHONY: qemu-deploy
qemu-deploy: qemu-image qemu-clab qemu-launch qemu-setup qemu-load-image deploy-controller ## Load image and deploy controller in the QEMU VM

.PHONY: qemu-e2etests
qemu-e2etests: ginkgo kubectl create-export-logs ## Run QEMU e2e tests (VM must be running)
	$(GINKGO) -v $(GINKGO_ARGS) --timeout=1h \
	  ./e2etests/suite -- \
	  --kubectl=$(KUBECTL) --groutmode --qemu-mode \
	  --reporterpath=$(KIND_EXPORT_LOGS) \
	  $(TEST_ARGS)

.PHONY: qemu-collect-logs
qemu-collect-logs: create-export-logs ## Collect QEMU VM and FRR logs
	$(QEMU_DIR)/vm/collect-logs.sh

.PHONY: qemu-clean
qemu-clean: ## Tear down the QEMU VM and clab topology (preserves disk image)
	$(QEMU_DIR)/vm/stop.sh

.PHONY: qemu-destroy
qemu-destroy: ## Fully destroy the VM, disk image, SSH keys, and clab topology
	$(QEMU_DIR)/vm/stop.sh --destroy

.PHONY: qemu-clab
qemu-clab: ## Deploy containerlab topology for QEMU
	$(QEMU_DIR)/deploy-clab.sh
