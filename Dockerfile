FROM scratch
MAINTAINER MinIO Development "dev@min.io"

EXPOSE 9999
EXPOSE 10000

COPY mperf /mperf

ENTRYPOINT ["/mperf"]
