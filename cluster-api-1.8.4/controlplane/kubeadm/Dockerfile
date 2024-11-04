
# Production image
FROM harbor.metastonecorp.com/front-end-learn/ubuntu:22.04_net
WORKDIR /

RUN mkdir -p /metastone
ADD manager /metastone

RUN echo "net.ipv4.tcp_mtu_probing=1" >> /etc/sysctl.conf

RUN sysctl -p

ENTRYPOINT ["/metastone/manager"]
