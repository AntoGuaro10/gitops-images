FROM bash:4.3

RUN apk update && apk add --no-cache curl net-tools busybox-extras

RUN adduser -D -u 10001 appuser
USER 10001

CMD ["bash"]
