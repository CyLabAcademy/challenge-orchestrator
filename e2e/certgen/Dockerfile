FROM alpine:3.22
RUN apk add --no-cache bash openssl
COPY distribute.sh /usr/local/bin/distribute.sh
ENTRYPOINT ["bash", "/usr/local/bin/distribute.sh"]
