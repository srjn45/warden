#!/usr/bin/env bash
# One-time setup: create a self-signed code-signing certificate in the login
# keychain so the warden binary has a STABLE code identity across rebuilds.
#
# Why this exists:
#   warden's daemon (under launchd) is the macOS TCC "responsible process" for
#   the agents it spawns and for its own /fs/dirs directory picker. When either
#   reads a protected folder (Downloads, Documents, Desktop, the Music/media
#   library), macOS shows a "warden would like to access…" consent prompt.
#   Granting Full Disk Access once silences these — but TCC keys the grant to the
#   binary's code identity. An UNSIGNED Go binary gets a fresh ad-hoc cdhash on
#   every rebuild, which invalidates the grant and brings the prompts back.
#
#   Signing with a fixed self-signed cert + stable identifier gives the binary an
#   identity-based Designated Requirement that survives rebuilds, so the Full Disk
#   Access grant sticks. This script creates that cert; common.sh's
#   codesign_binary() does the signing on every install.
#
# Idempotent: re-running when the cert already exists is a no-op.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/common.sh
source "$SCRIPT_DIR/common.sh"

KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

if security find-certificate -c "$CODESIGN_IDENTITY" >/dev/null 2>&1; then
  info "code-signing certificate '$CODESIGN_IDENTITY' already exists — nothing to do"
  exit 0
fi

command -v openssl >/dev/null 2>&1 || die "openssl not found (brew install openssl)"

info "creating self-signed code-signing certificate '$CODESIGN_IDENTITY'…"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/req.cnf" <<EOF
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = $CODESIGN_IDENTITY
[v3]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
EOF

openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout "$tmp/key.pem" -out "$tmp/cert.pem" -config "$tmp/req.cnf" \
  || die "openssl failed to generate the certificate"

# -legacy + a non-empty passphrase: OpenSSL 3 defaults to a PKCS#12 MAC that
# macOS's Security.framework cannot verify (and empty-password p12s fail
# outright), so the bundle must use the legacy SHA1 MAC / 3DES encoding.
p12pass="warden-import"
openssl pkcs12 -export -legacy -inkey "$tmp/key.pem" -in "$tmp/cert.pem" \
  -out "$tmp/cert.p12" -passout "pass:$p12pass" -name "$CODESIGN_IDENTITY" \
  || die "openssl failed to package the certificate"

# Import cert+key into the login keychain. -T /usr/bin/codesign pre-authorizes
# codesign to use the private key; macOS may still prompt "Always Allow" on the
# first signing — click it once.
security import "$tmp/cert.p12" -k "$KEYCHAIN" -P "$p12pass" -T /usr/bin/codesign \
  || die "failed to import certificate into the login keychain"

info "imported '$CODESIGN_IDENTITY' into the login keychain"

# Best-effort: let codesign use the key without a GUI prompt. Needs the keychain
# (login) password; if it can't get it non-interactively, signing still works —
# codesign just prompts once with an "Always Allow" button.
if security set-key-partition-list -S apple-tool:,apple:,codesign: -s \
     -k "" "$KEYCHAIN" >/dev/null 2>&1; then
  info "configured key access for codesign (no prompts)"
else
  warn "could not pre-authorize the key non-interactively;"
  warn "  the first signing may show a keychain prompt — click \"Always Allow\"."
fi

info "done. Next steps:"
cat <<EOF

  1. Reinstall so the binary gets signed with the new identity:
         ./scripts/install.sh

  2. Grant warden Full Disk Access ONCE:
         System Settings → Privacy & Security → Full Disk Access → "+"
         add:  $INSTALL_BIN
     (Toggle it on. You can drag the binary in, or ⌘⇧G and paste the path.)

  After that the access prompts stop, and rebuilds keep the grant because the
  binary's code identity is now stable.
EOF
