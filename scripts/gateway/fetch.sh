#!/usr/bin/env bash
# Fixed upstream release, with an independently published release asset digest.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
dest="$root/simplecache-operator/bin/gateway-v1.9.2"
mkdir -p "$dest"
file="$dest/install.yaml"
checksum=0412a72907e57ff9b73c56a7bf6df5190bf0f6e4f8bb4bba34e38630bbab5778
verify() { echo "$checksum  $file" | sha256sum --check --status; }
if [[ ! -f "$file" ]] || ! verify; then
 tmp=$(mktemp "$dest/download.XXXXXX")
 trap 'rm -f -- "$tmp"' EXIT
 curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
  https://github.com/envoyproxy/gateway/releases/download/v1.9.2/install.yaml -o "$tmp"
 echo "$checksum  $tmp" | sha256sum --check --status
 mv -- "$tmp" "$file"
fi
# Extract only official CRDs used by envtest; retain the full bundle for Kind.
python3 - "$file" "$dest" <<'PY'
import pathlib, sys
# Upstream multi-document YAML has document delimiters. No third-party Python
# packages are required for extraction; each selected document is kept verbatim.
src=pathlib.Path(sys.argv[1]).read_text()
names={'gatewayclasses.gateway.networking.k8s.io','gateways.gateway.networking.k8s.io',
       'httproutes.gateway.networking.k8s.io','envoyproxies.gateway.envoyproxy.io',
       'backends.gateway.envoyproxy.io','backendtrafficpolicies.gateway.envoyproxy.io'}
selected=[]
for doc in src.split('\n---\n'):
    if '\nkind: CustomResourceDefinition\n' not in '\n'+doc+'\n': continue
    for name in names:
        if '\n  name: '+name+'\n' in doc:
            selected.append(doc); names.remove(name); break
if names: raise SystemExit('Missing upstream CRDs: '+str(names))
crds=pathlib.Path(sys.argv[2])/'crds'
crds.mkdir(exist_ok=True)
(crds/'gateway.yaml').write_text('\n---\n'.join(selected))
PY

if [[ "${1:-}" == --kind ]]; then
  case "$(uname -s)/$(uname -m)" in
    Linux/x86_64) arch=amd64; kind_sum=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
    Linux/aarch64|Linux/arm64) arch=arm64; kind_sum=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 ;;
    *) echo 'Gateway smoke supports Linux amd64/arm64' >&2; exit 2 ;;
  esac
  kind_bin="$dest/kind-v0.33.0"
  if [[ ! -f "$kind_bin" ]] || ! echo "$kind_sum  $kind_bin" | sha256sum --check --status; then
    tmp=$(mktemp "$dest/download.XXXXXX")
    trap 'rm -f -- "$tmp"' EXIT
    curl --fail --location --retry 2 --connect-timeout 15 --max-time 180 \
      "https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-$arch" -o "$tmp"
    echo "$kind_sum  $tmp" | sha256sum --check --status
    chmod +x "$tmp"
    mv -- "$tmp" "$kind_bin"
  fi
fi
