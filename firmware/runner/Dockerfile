FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends iproute2 python3 qemu-user-static \
 && rm -rf /var/lib/apt/lists/*
