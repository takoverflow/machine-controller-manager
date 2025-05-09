# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

ifeq ($(strip $(shell go list -m 2>/dev/null)),github.com/gardener/machine-controller-manager)
TOOLS_PKG_PATH             := ./hack/tools
else
# dependency on github.com/gardener/machine-controller-manager/hack/tools is optional and only needed if other projects want to reuse
# install-gosec.sh. If they don't use it and the project doesn't depend on the package,
# silence the error to minimize confusion.
TOOLS_PKG_PATH             := $(shell go list -tags tools -f '{{ .Dir }}' github.com/gardener/machine-controller-manager/hack/tools 2>/dev/null)
endif
TOOLS_BIN_DIR              := $(TOOLS_DIR)/bin

## Tool Binaries
CONTROLLER_GEN ?= $(TOOLS_BIN_DIR)/controller-gen
DEEPCOPY_GEN ?= $(TOOLS_BIN_DIR)/deepcopy-gen
DEFAULTER_GEN ?= $(TOOLS_BIN_DIR)/defaulter-gen
CONVERSION_GEN ?= $(TOOLS_BIN_DIR)/conversion-gen
OPENAPI_GEN ?= $(TOOLS_BIN_DIR)/openapi-gen
VGOPATH ?= $(TOOLS_BIN_DIR)/vgopath
GEN_CRD_API_REFERENCE_DOCS ?= $(TOOLS_BIN_DIR)/gen-crd-api-reference-docs
GO_ADD_LICENSE ?= $(TOOLS_BIN_DIR)/addlicense
GOIMPORTS ?= $(TOOLS_BIN_DIR)/goimports
GOLANGCI_LINT ?= $(TOOLS_BIN_DIR)/golangci-lint
GOSEC ?= $(TOOLS_BIN_DIR)/gosec

## Tool Versions
CODE_GENERATOR_VERSION ?= v0.31.0
VGOPATH_VERSION ?= v0.1.6
CONTROLLER_TOOLS_VERSION ?= v0.16.1
GEN_CRD_API_REFERENCE_DOCS_VERSION ?= v0.3.0
ADDLICENSE_VERSION ?= v1.1.1
GOIMPORTS_VERSION ?= v0.13.0
GOLANGCI_LINT_VERSION ?= v1.64.8
GOSEC_VERSION ?= v2.21.4


# default tool versions
GO_ADD_LICENSE_VERSION ?= latest

export TOOLS_BIN_DIR := $(TOOLS_BIN_DIR)
export PATH := $(abspath $(TOOLS_BIN_DIR)):$(PATH)

#########################################
# Tools                                 #
#########################################

$(GO_ADD_LICENSE):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install github.com/google/addlicense@$(GO_ADD_LICENSE_VERSION)

$(CONTROLLER_GEN):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(DEEPCOPY_GEN):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install k8s.io/code-generator/cmd/deepcopy-gen@$(CODE_GENERATOR_VERSION)

$(DEFAULTER_GEN):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install k8s.io/code-generator/cmd/defaulter-gen@$(CODE_GENERATOR_VERSION)

$(CONVERSION_GEN):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install k8s.io/code-generator/cmd/conversion-gen@$(CODE_GENERATOR_VERSION)

$(OPENAPI_GEN):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install k8s.io/kube-openapi/cmd/openapi-gen

$(VGOPATH):
	@if test -x $(TOOLS_BIN_DIR)/vgopath && ! $(TOOLS_BIN_DIR)/vgopath version | grep -q $(VGOPATH_VERSION); then \
		echo "$(TOOLS_BIN_DIR)/vgopath version is not expected $(VGOPATH_VERSION). Removing it before installing."; \
		rm -rf $(TOOLS_BIN_DIR)/vgopath; \
	fi
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install github.com/ironcore-dev/vgopath@$(VGOPATH_VERSION)

$(GEN_CRD_API_REFERENCE_DOCS):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install github.com/ahmetb/gen-crd-api-reference-docs@$(GEN_CRD_API_REFERENCE_DOCS_VERSION)

$(GOIMPORTS):
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

$(GOLANGCI_LINT): $(TOOLS_BIN_DIR)
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOSEC):
	GOSEC_VERSION=$(GOSEC_VERSION) bash $(TOOLS_PKG_PATH)/install-gosec.sh
