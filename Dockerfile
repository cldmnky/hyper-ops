# Use distroless as minimal base image to package the zupd binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
ARG TARGETPLATFORM
LABEL PROJECT="Hyper-Ops" \
      MAINTAINER="mbengtss@redhat.com" \
      DESCRIPTION="Hyper-Ops Controller" \
      LICENSE="Apache-2.0" \
      PLATFORM="$TARGETPLATFORM" \
      VCS_URL="github.com/cldmnky/hyper-ops" \
      COMPONENT="hyper-ops controller"
WORKDIR /
COPY ${TARGETPLATFORM}/hyper-ops /hyper-ops
USER 65532:65532
ENTRYPOINT ["/hyper-ops"]