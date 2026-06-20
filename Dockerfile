FROM golang@sha256:87a41d2539e5671777734e91f467499ed5eafb1fb1f77221dff2744db7a51775 AS builder

WORKDIR /usr/src/app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-update cmd/update/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-migrate cmd/migrate/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-revalidate cmd/revalidate/main.go

FROM alpine@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

ARG USER_UID=1000
ARG USER_GID=1000

RUN --mount=type=cache,target=/var/cache/apk \
	apk add --update-cache subversion

RUN addgroup -S loki && adduser -S loki -G loki -u ${USER_UID} -g ${USER_GID} \
	&& mkdir -p /code \
	&& chown loki:loki /code

COPY --from=trywpm/cli:latest / /usr/local/bin/
COPY --from=builder /usr/src/app/wpm-update /usr/local/bin/update-wpm
COPY --from=builder /usr/src/app/wpm-migrate /usr/local/bin/migrate-wpm
COPY --from=builder /usr/src/app/wpm-revalidate /usr/local/bin/revalidate-wpm

COPY entrypoint/update.sh /usr/local/bin/update
COPY entrypoint/migrate.sh /usr/local/bin/migrate
COPY entrypoint/revalidate.sh /usr/local/bin/revalidate
COPY entrypoint/backfill-migrate.sh /usr/local/bin/backfill-migrate
COPY entrypoint/migrate-by-name.sh /usr/local/bin/migrate-by-name

RUN chmod +x /usr/local/bin/update
RUN chmod +x /usr/local/bin/migrate
RUN chmod +x /usr/local/bin/revalidate
RUN chmod +x /usr/local/bin/backfill-migrate
RUN chmod +x /usr/local/bin/migrate-by-name

USER loki

WORKDIR /code

CMD ["/usr/local/bin/migrate"]
