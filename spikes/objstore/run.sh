#!/usr/bin/env bash
# Runs the object store spike against disposable SeaweedFS and Garage containers.
# Usage: run.sh <spike_binary> <workdir>
set -uo pipefail
spike=$1
work=$2
mkdir -p "$work"
cleanup() { docker rm -f jellyscale-spike-sw jellyscale-spike-garage >/dev/null 2>&1; }
trap cleanup EXIT
cleanup

echo "=== SeaweedFS ==="
cat >"$work/s3.json" <<'E'
{"identities":[{"name":"spike","credentials":[{"accessKey":"spikekey","secretKey":"spikesecret"}],"actions":["Admin","Read","Write","List"]}]}
E
docker run -d --name jellyscale-spike-sw -p 127.0.0.1::8333 -v "$work/s3.json:/s3.json:ro" \
  chrislusf/seaweedfs:latest server -s3 -s3.config=/s3.json -volume.max=20 -dir=/data >/dev/null
port=$(docker port jellyscale-spike-sw 8333/tcp | head -1 | sed 's/.*://')
for _ in $(seq 1 60); do curl -s -m1 -o /dev/null "http://127.0.0.1:$port/" && break; sleep 1; done
sleep 4
docker exec jellyscale-spike-sw sh -c 'echo "s3.bucket.create -name spike" | weed shell' >/dev/null 2>&1
sleep 2
"$spike" -endpoint "http://127.0.0.1:$port" -key spikekey -secret spikesecret 2>&1 | sed 's/URL .*/URL ok/'

echo "=== Garage ==="
cat >"$work/garage.toml" <<'E'
metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "lmdb"
replication_factor = 1
rpc_bind_addr = "0.0.0.0:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "0000000000000000000000000000000000000000000000000000000000000001"
[s3_api]
s3_region = "garage"
api_bind_addr = "0.0.0.0:3900"
E
docker run -d --name jellyscale-spike-garage -p 127.0.0.1::3900 -v "$work/garage.toml:/etc/garage.toml:ro" \
  dxflrs/garage:v2.1.0 >/dev/null
port=$(docker port jellyscale-spike-garage 3900/tcp | head -1 | sed 's/.*://')
sleep 4
g() { docker exec jellyscale-spike-garage /garage "$@"; }
node=$(g status 2>/dev/null | awk '/^[0-9a-f]{16}/{print $1; exit}')
g layout assign -z dc1 -c 10G "$node" >/dev/null 2>&1
g layout apply --version 1 >/dev/null 2>&1
g bucket create spike >/dev/null 2>&1
creds=$(g key create spike-key 2>/dev/null)
kid=$(echo "$creds" | awk '/Key ID/{print $NF}')
sec=$(echo "$creds" | awk '/Secret key/{print $NF}')
g bucket allow --read --write --owner spike --key spike-key >/dev/null 2>&1
"$spike" -endpoint "http://127.0.0.1:$port" -key "$kid" -secret "$sec" -region garage 2>&1 | sed 's/URL .*/URL ok/'
