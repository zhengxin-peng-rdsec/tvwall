# Build a static server binary and run it as an unprivileged container user.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum* ./server/
COPY server ./server
WORKDIR /src/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -o /out/tvwall-server .

FROM nginx:alpine
COPY --from=build /out/tvwall-server /usr/local/bin/tvwall-server
RUN mkdir -p /data/frames && chown -R nginx:nginx /data /usr/local/bin/tvwall-server
ENV TVWALL_DATA=/data
EXPOSE 8088
USER nginx:nginx
ENTRYPOINT ["/usr/local/bin/tvwall-server"]
CMD []
