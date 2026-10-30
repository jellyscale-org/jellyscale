# Starts disposable S3-compatible stores. Source this file.
# start_seaweed <container> <workdir> <bucket>; sets seaweed_port, _key, _secret.
start_seaweed() {
  local name=$1 work=$2 bucket=$3
  seaweed_key=itkey seaweed_secret=itsecret
  cat >"$work/s3.json" <<J
{"identities":[{"name":"it","credentials":[{"accessKey":"$seaweed_key","secretKey":"$seaweed_secret"}],"actions":["Admin","Read","Write","List"]}]}
J
  docker run -d --rm --name "$name" -p 127.0.0.1::8333 -v "$work/s3.json:/s3.json:ro" \
    chrislusf/seaweedfs:latest server -s3 -s3.config=/s3.json -volume.max=20 -dir=/data >/dev/null || return 1
  seaweed_port=$(docker port "$name" 8333/tcp | head -1 | sed 's/.*://')
  for _ in $(seq 1 60); do curl -s -m1 -o /dev/null "http://127.0.0.1:$seaweed_port/" && break; sleep 1; done
  sleep 4
  docker exec "$name" sh -c "echo 's3.bucket.create -name $bucket' | weed shell" >/dev/null 2>&1
  sleep 2
}

# start_garage <container> <workdir> <bucket>; sets garage_port, _key, _secret.
start_garage() {
  local name=$1 work=$2 bucket=$3 node creds
  cat >"$work/garage.toml" <<T
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
T
  docker run -d --rm --name "$name" -p 127.0.0.1::3900 -v "$work/garage.toml:/etc/garage.toml:ro" dxflrs/garage:v2.1.0 >/dev/null || return 1
  garage_port=$(docker port "$name" 3900/tcp | head -1 | sed 's/.*://')
  sleep 4
  node=$(docker exec "$name" /garage status 2>/dev/null | awk '/^[0-9a-f]{16}/{print $1; exit}')
  docker exec "$name" /garage layout assign -z dc1 -c 10G "$node" >/dev/null 2>&1
  docker exec "$name" /garage layout apply --version 1 >/dev/null 2>&1
  docker exec "$name" /garage bucket create "$bucket" >/dev/null 2>&1
  creds=$(docker exec "$name" /garage key create it-key 2>/dev/null)
  garage_key=$(echo "$creds" | awk '/Key ID/{print $NF}')
  garage_secret=$(echo "$creds" | awk '/Secret key/{print $NF}')
  docker exec "$name" /garage bucket allow --read --write --owner "$bucket" --key it-key >/dev/null 2>&1
}
