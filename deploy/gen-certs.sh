#!/usr/bin/env bash
# Creates a throwaway CA and serving certificate for the webhook Service,
# stores them in the kube-fence-tls Secret, and prints the base64 CA bundle.
set -euo pipefail

NAMESPACE=${NAMESPACE:-kube-fence}
SERVICE=${SERVICE:-kube-fence}
KUBECTL=${KUBECTL:-kubectl}
DIR=${CERT_DIR:-$(cd "$(dirname "$0")" && pwd)/certs}

mkdir -p "$DIR"
cd "$DIR"

openssl genrsa -out ca.key 2048 2>/dev/null
openssl req -x509 -new -nodes -key ca.key -sha256 -days 365 -subj "/CN=${SERVICE}-ca" -out ca.crt

openssl genrsa -out tls.key 2048 2>/dev/null
openssl req -new -key tls.key -subj "/CN=${SERVICE}.${NAMESPACE}.svc" -out tls.csr
cat > ext.cnf <<EXT
subjectAltName=DNS:${SERVICE},DNS:${SERVICE}.${NAMESPACE},DNS:${SERVICE}.${NAMESPACE}.svc,DNS:${SERVICE}.${NAMESPACE}.svc.cluster.local
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EXT
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -sha256 -days 365 -extfile ext.cnf -out tls.crt 2>/dev/null

$KUBECTL create secret tls kube-fence-tls -n "$NAMESPACE" --cert=tls.crt --key=tls.key \
  --dry-run=client -o yaml | $KUBECTL apply -f - >&2

base64 < ca.crt | tr -d '\n'
