FROM golang@sha256:36b4f45d2874905b9e8573b783292629bcb346d0a70d8d7150b6df545234818f AS builder

WORKDIR /usr/src/app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-update cmd/update/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-migrate cmd/migrate/main.go

FROM alpine@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62

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

COPY update.sh /usr/local/bin/update
COPY migrate.sh /usr/local/bin/migrate

RUN chmod +x /usr/local/bin/update
RUN chmod +x /usr/local/bin/migrate

USER loki

WORKDIR /code

CMD ["/usr/local/bin/migrate"]
