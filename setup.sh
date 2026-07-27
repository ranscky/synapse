#!/usr/bin/env bash
# setup.sh — Synapse Context Compiler setup script
# Downloads and installs the ONNX Runtime native library and the
# all-MiniLM-L6-v2 embedding model for your platform.
#
# Usage:
#   bash setup.sh           # interactive
#   bash setup.sh --yes     # non-interactive (accept all defaults)
#
# Supported platforms:
#   Linux   x86_64, aarch64
#   macOS   x86_64 (Intel), arm64 (Apple Silicon)
#   Windows x86_64 (via Git Bash or WSL)

set -euo pipefail

ONNX_VERSION="1.27.0"
MODEL_DIR="models/all-MiniLM-L6-v2"
# Pinned to commit ca2c6b8 -- see ci.yml/release.yml for why: main's
# current onnx/model.onnx is a different export backend that lacks the
# sentence_embedding output this project's embedder requires by name.
MODEL_REVISION="ca2c6b84644f6c2f4b7c879851ad9d364ae375f6"
MODEL_URL="https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/${MODEL_REVISION}/onnx/model.onnx"
VOCAB_URL="https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/${MODEL_REVISION}/vocab.txt"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

info()    { echo -e "${GREEN}[setup]${NC} $*"; }
warn()    { echo -e "${YELLOW}[warn]${NC}  $*"; }
error()   { echo -e "${RED}[error]${NC} $*" >&2; exit 1; }

# ─── Detect platform ──────────────────────────────────────────────────────────

detect_platform() {
    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux)
            case "$ARCH" in
                x86_64)  ONNX_PLATFORM="linux-x64" ;;
                aarch64) ONNX_PLATFORM="linux-aarch64" ;;
                *) error "Unsupported Linux architecture: $ARCH" ;;
            esac
            LIB_NAME="libonnxruntime.so.${ONNX_VERSION}"
            LIB_DEST="/usr/local/lib"
            NEEDS_SUDO=true
            ;;
        Darwin)
            case "$ARCH" in
                x86_64) ONNX_PLATFORM="osx-x86_64" ;;
                arm64)  ONNX_PLATFORM="osx-arm64" ;;
                *) error "Unsupported macOS architecture: $ARCH" ;;
            esac
            LIB_NAME="libonnxruntime.${ONNX_VERSION}.dylib"
            LIB_DEST="/usr/local/lib"
            NEEDS_SUDO=true
            ;;
        MINGW*|MSYS*|CYGWIN*)
            ONNX_PLATFORM="win-x64"
            LIB_NAME="onnxruntime.dll"
            LIB_DEST="$(pwd)"   # Windows: place alongside the binary
            NEEDS_SUDO=false
            warn "Windows detected. The DLL will be placed in the current directory."
            warn "If you move the synapse binary, copy onnxruntime.dll alongside it."
            ;;
        *) error "Unsupported OS: $OS" ;;
    esac

    ONNX_ARCHIVE="onnxruntime-${ONNX_PLATFORM}-${ONNX_VERSION}"
    ONNX_URL="https://github.com/microsoft/onnxruntime/releases/download/v${ONNX_VERSION}/${ONNX_ARCHIVE}.tgz"

    info "Platform: $OS $ARCH → $ONNX_PLATFORM"
}

# ─── Check prerequisites ──────────────────────────────────────────────────────

check_prereqs() {
    local missing=()
    command -v go   >/dev/null 2>&1 || missing+=("go (https://go.dev/dl)")
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || missing+=("curl or wget")
    command -v tar  >/dev/null 2>&1 || missing+=("tar")

    if [[ ${#missing[@]} -gt 0 ]]; then
        error "Missing required tools:\n  ${missing[*]}"
    fi

    GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    REQUIRED="1.22"
    if [[ "$(printf '%s\n' "$REQUIRED" "$GO_VERSION" | sort -V | head -n1)" != "$REQUIRED" ]]; then
        error "Go $REQUIRED+ required, found $GO_VERSION"
    fi

    info "Prerequisites OK (Go $GO_VERSION)"
}

# ─── Download helper ──────────────────────────────────────────────────────────

download() {
    local url="$1" dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$dest"
    else
        wget -q "$url" -O "$dest"
    fi
}

# ─── Install ONNX Runtime ─────────────────────────────────────────────────────

install_onnx() {
    # Check if already installed
    if ldconfig -p 2>/dev/null | grep -q "libonnxruntime" || \
       ls "${LIB_DEST}/${LIB_NAME}" 2>/dev/null | grep -q .; then
        info "ONNX Runtime already installed — skipping"
        return
    fi

    info "Downloading ONNX Runtime v${ONNX_VERSION} for ${ONNX_PLATFORM}..."
    local tmp
    tmp="$(mktemp -d)"
    trap "rm -rf $tmp" EXIT

    download "$ONNX_URL" "${tmp}/${ONNX_ARCHIVE}.tgz"

    info "Extracting..."
    tar -xzf "${tmp}/${ONNX_ARCHIVE}.tgz" -C "$tmp"

    local lib_src
    lib_src="$(find "$tmp" -name "libonnxruntime*" -o -name "onnxruntime.dll" 2>/dev/null | head -n1)"
    [[ -z "$lib_src" ]] && error "Could not find ONNX Runtime library in archive"

    info "Installing to ${LIB_DEST}..."
    if [[ "$NEEDS_SUDO" == "true" ]]; then
        sudo cp "$lib_src" "${LIB_DEST}/"
        # Create versioned symlinks on Linux
        if [[ "$OS" == "Linux" ]]; then
            sudo ln -sf "${LIB_DEST}/${LIB_NAME}" "${LIB_DEST}/libonnxruntime.so.1"
            sudo ln -sf "${LIB_DEST}/libonnxruntime.so.1" "${LIB_DEST}/libonnxruntime.so"
            sudo ldconfig
        fi
        # Create symlink on macOS
        if [[ "$OS" == "Darwin" ]]; then
            sudo ln -sf "${LIB_DEST}/${LIB_NAME}" "${LIB_DEST}/libonnxruntime.dylib"
        fi
    else
        cp "$lib_src" "${LIB_DEST}/"
    fi

    info "ONNX Runtime installed ✓"
}

# ─── Download embedding model ─────────────────────────────────────────────────

install_model() {
    mkdir -p "$MODEL_DIR"

    if [[ -f "${MODEL_DIR}/model.onnx" && -f "${MODEL_DIR}/vocab.txt" ]]; then
        info "Model already present at ${MODEL_DIR}/ — skipping"
        return
    fi

    info "Downloading all-MiniLM-L6-v2 model (~90MB)..."
    download "$MODEL_URL" "${MODEL_DIR}/model.onnx"

    info "Downloading vocabulary file..."
    download "$VOCAB_URL" "${MODEL_DIR}/vocab.txt"

    info "Model downloaded ✓"
}

# ─── Build Synapse ────────────────────────────────────────────────────────────

build_synapse() {
    info "Downloading Go dependencies..."
    go mod download

    info "Building synapse binary..."
    go build -o synapse ./cmd/synapse
    info "Build complete ✓ — binary: ./synapse"
}

# ─── Config scaffold ──────────────────────────────────────────────────────────

scaffold_config() {
    if [[ -f "synapse.yaml" ]]; then
        info "synapse.yaml already exists — skipping"
        return
    fi

    if [[ -f "synapse.yaml.example" ]]; then
        cp synapse.yaml.example synapse.yaml
        chmod 600 synapse.yaml
        info "Created synapse.yaml from example (edit before running)"
    fi
}

# ─── Main ─────────────────────────────────────────────────────────────────────

main() {
    echo ""
    echo "  Synapse Context Compiler — Setup"
    echo "  ================================"
    echo ""

    detect_platform
    check_prereqs
    install_onnx
    install_model
    build_synapse
    scaffold_config

    echo ""
    info "Setup complete. Next steps:"
    echo ""
    echo "  1. Edit synapse.yaml — set your upstream model URL"
    echo "  2. Run: ./synapse --config synapse.yaml"
    echo "  3. Point your AI client (Cline, Open WebUI, etc.) at http://127.0.0.1:8080"
    echo ""
    echo "  Docs: README.md"
    echo "  API:  ./synapse --config synapse.yaml, then GET /openapi.yaml"
    echo ""
}

main "$@"