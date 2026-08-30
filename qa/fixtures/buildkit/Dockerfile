FROM alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS dependencies
WORKDIR /work
COPY dependency.txt ./
RUN sha256sum dependency.txt > dependency.sha256 && sleep 1

FROM dependencies AS build
COPY source.txt ./
RUN sha256sum source.txt > source.sha256 && cat dependency.sha256 source.sha256 > result.txt && sleep 1

FROM alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
COPY --from=build /work/result.txt /result.txt
CMD ["cat", "/result.txt"]
