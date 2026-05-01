#!/bin/bash
# install.sh — Install qai-cli and optional dependencies.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/quantum-encoding/qai-cli/master/install.sh | bash
#   curl -sSL ... | bash -s -- --all        # install everything
#   curl -sSL ... | bash -s -- --minimal    # just the binary
#
# Or run locally:
#   ./install.sh              # interactive — pick what you want
#   ./install.sh --all        # install everything
#   ./install.sh --minimal    # just the qai binary

set -euo pipefail

# ─── Colors ────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

ok()   { echo -e "  ${GREEN}✓${NC} $1"; }
skip() { echo -e "  ${YELLOW}–${NC} $1 (skipped)"; }
fail() { echo -e "  ${RED}✗${NC} $1"; }
info() { echo -e "  ${BLUE}ℹ${NC} $1"; }

# ─── Detect OS / Arch ─────────────────────────────────────────────────────

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac

INSTALL_DIR="${HOME}/.local/bin"
mkdir -p "$INSTALL_DIR"

# Ensure ~/.local/bin is on PATH
if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
  info "Add to your shell profile: export PATH=\"\$HOME/.local/bin:\$PATH\""
fi

# ─── Parse args ────────────────────────────────────────────────────────────

MODE="interactive"
for arg in "$@"; do
  case "$arg" in
    --all)     MODE="all" ;;
    --minimal) MODE="minimal" ;;
    --help|-h)
      echo "Usage: install.sh [--all | --minimal]"
      echo "  --all      Install qai + all optional dependencies"
      echo "  --minimal  Install only the qai binary"
      echo "  (default)  Interactive — choose what to install"
      exit 0
      ;;
  esac
done

# ─── Banner ────────────────────────────────────────────────────────────────

echo ""
echo -e "${BOLD}╔══════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║         qai-cli installer                ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════╝${NC}"
echo ""
echo -e "  OS: ${CYAN}${OS}/${ARCH}${NC}"
echo -e "  Install dir: ${CYAN}${INSTALL_DIR}${NC}"
echo ""

# ─── Helper: check if command exists ───────────────────────────────────────

has() { command -v "$1" &>/dev/null; }

# ─── Helper: ask y/n ──────────────────────────────────────────────────────

ask() {
  if [[ "$MODE" == "all" ]]; then return 0; fi
  if [[ "$MODE" == "minimal" ]]; then return 1; fi
  local prompt="$1"
  local default="${2:-y}"
  local hint="[Y/n]"
  [[ "$default" == "n" ]] && hint="[y/N]"
  read -rp "  $prompt $hint: " answer
  answer="${answer:-$default}"
  [[ "${answer,,}" == "y" ]]
}

# ─── 1. Install qai binary ────────────────────────────────────────────────

echo -e "${BOLD}[1/7] qai binary${NC}"

if has go; then
  echo "  Building from source..."
  go install github.com/quantum-encoding/qai-cli@latest 2>/dev/null
  QAI_BIN="$(go env GOPATH)/bin/qai-cli"
  if [[ -f "$QAI_BIN" ]]; then
    # Use `install(1)` rather than `cp`: cp on macOS causes Gatekeeper
    # to silently SIGKILL the destination binary on first run (exit 137,
    # no output, no error message). install(1) creates a fresh inode
    # with metadata Gatekeeper treats as benign. Empirically verified
    # — the xattr difference between the two paths is the same, but
    # only cp triggers the kill. Strip xattrs anyway as belt-and-braces.
    install -m 755 "$QAI_BIN" "${INSTALL_DIR}/qai"
    if [[ "$OS" == "darwin" ]]; then
      xattr -d com.apple.quarantine "${INSTALL_DIR}/qai" 2>/dev/null || true
      xattr -d com.apple.provenance "${INSTALL_DIR}/qai" 2>/dev/null || true
      xattr -c "${INSTALL_DIR}/qai" 2>/dev/null || true
    fi
    ok "qai installed ($(${INSTALL_DIR}/qai help 2>&1 | head -1))"
  else
    fail "go install failed"
    exit 1
  fi
else
  fail "Go not found — install from https://go.dev/dl/"
  echo "  After installing Go, re-run this script."
  exit 1
fi

echo ""

# ─── 2. SurrealDB (local knowledge base) ──────────────────────────────────

echo -e "${BOLD}[2/7] SurrealDB${NC} — local vector database for qai db / qai search --local"

if has surreal; then
  ok "surreal already installed ($(surreal version 2>/dev/null || echo 'unknown'))"
elif ask "Install SurrealDB?"; then
  case "$OS" in
    darwin)
      if has brew; then
        brew install surrealdb/tap/surreal
      else
        curl -sSf https://install.surrealdb.com | sh -s -- --install-path "$INSTALL_DIR"
      fi
      ;;
    linux)
      curl -sSf https://install.surrealdb.com | sh -s -- --install-path "$INSTALL_DIR"
      ;;
    *)
      info "Visit https://surrealdb.com/install for your platform"
      ;;
  esac
  if has surreal; then
    ok "surreal installed"
  else
    fail "surreal install failed — visit https://surrealdb.com/install"
  fi
else
  skip "SurrealDB"
fi

echo ""

# ─── 3. Ollama (local embeddings) ─────────────────────────────────────────

echo -e "${BOLD}[3/7] Ollama${NC} — local embedding model (zero API keys)"

if has ollama; then
  ok "ollama already installed"
  # Pull embedding model if not present
  if ! ollama list 2>/dev/null | grep -q "nomic-embed-text"; then
    if ask "Pull nomic-embed-text embedding model?"; then
      ollama pull nomic-embed-text
      ok "nomic-embed-text model pulled"
    fi
  else
    ok "nomic-embed-text model available"
  fi
elif ask "Install Ollama? (for fully local embeddings, no API key needed)"; then
  case "$OS" in
    darwin)
      info "Opening Ollama download page..."
      open "https://ollama.ai/download"
      info "After installing, run: ollama pull nomic-embed-text"
      ;;
    linux)
      curl -fsSL https://ollama.ai/install.sh | sh
      if has ollama; then
        ok "ollama installed"
        ollama pull nomic-embed-text &
        info "Pulling nomic-embed-text model in background..."
      fi
      ;;
  esac
else
  skip "Ollama (you can use Gemini or OpenAI API keys instead)"
fi

echo ""

# ─── 4. Graphviz (for qai graph) ──────────────────────────────────────────

echo -e "${BOLD}[4/7] Graphviz${NC} — for qai graph (call/dependency graph SVG)"

if has dot; then
  ok "graphviz already installed"
elif ask "Install Graphviz?" "n"; then
  case "$OS" in
    darwin) brew install graphviz 2>/dev/null || info "brew install graphviz" ;;
    linux)
      if has apt-get; then sudo apt-get install -y graphviz
      elif has dnf; then sudo dnf install -y graphviz
      elif has pacman; then sudo pacman -S --noconfirm graphviz
      else info "Install graphviz via your package manager"
      fi
      ;;
  esac
  has dot && ok "graphviz installed" || fail "graphviz install failed"
else
  skip "Graphviz (qai graph won't generate SVG)"
fi

echo ""

# ─── 5. tmux (for qai term) ───────────────────────────────────────────────

echo -e "${BOLD}[5/7] tmux${NC} — for qai term (terminal management)"

if has tmux; then
  ok "tmux already installed"
elif ask "Install tmux?" "n"; then
  case "$OS" in
    darwin) brew install tmux 2>/dev/null || info "brew install tmux" ;;
    linux)
      if has apt-get; then sudo apt-get install -y tmux
      elif has dnf; then sudo dnf install -y tmux
      elif has pacman; then sudo pacman -S --noconfirm tmux
      fi
      ;;
  esac
  has tmux && ok "tmux installed" || fail "tmux install failed"
else
  skip "tmux (qai term won't work)"
fi

echo ""

# ─── 6. Language analyzers (for qai analyze) ──────────────────────────────

echo -e "${BOLD}[6/7] Language analyzers${NC} — for qai analyze (multi-language code analysis)"

echo "  Checking language runtimes..."
has node    && ok "Node.js (TypeScript analyzer)" || skip "Node.js — qai analyze won't support TypeScript"
has python3 && ok "Python 3 (Python + Kotlin analyzer)" || skip "Python 3 — qai analyze won't support Python/Kotlin"
has swift   && ok "Swift (Swift analyzer)" || skip "Swift — qai analyze won't support Swift"

echo ""

# ─── 7. Run qai init ──────────────────────────────────────────────────────

echo -e "${BOLD}[7/7] Configuration${NC}"
echo ""

if [[ -f "${HOME}/.qai/config.yaml" ]]; then
  ok "Config already exists at ~/.qai/config.yaml"
  if ask "Re-run qai init to update configuration?" "n"; then
    "${INSTALL_DIR}/qai" init
  fi
else
  if ask "Run qai init to configure embedding provider and knowledge base?"; then
    "${INSTALL_DIR}/qai" init
  else
    info "Run 'qai init' later to set up your configuration"
  fi
fi

echo ""

# ─── Summary ──────────────────────────────────────────────────────────────

echo -e "${BOLD}════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Installation complete!${NC}"
echo ""
echo "  Installed to: ${INSTALL_DIR}/qai"
echo ""
echo "  Quick start:"
echo "    qai init                              # first-time setup"
echo "    qai db start                          # start local knowledge base"
echo "    qai ingest --local my-docs ~/Docs/    # embed documents"
echo "    qai search --local \"how does X work\"  # search"
echo "    qai browser launch                    # browser automation"
echo "    qai help                              # all commands"
echo ""

if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
  echo -e "  ${YELLOW}Add to your shell profile:${NC}"
  echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
  echo ""
fi

echo -e "${BOLD}════════════════════════════════════════════${NC}"
