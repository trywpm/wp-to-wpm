FROM golang@sha256:1e598ea5752ae26c093b746fd73c5095af97d6f2d679c43e83e0eac484a33dc3 AS builder

WORKDIR /usr/src/app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-update cmd/update/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-migrate cmd/migrate/main.go

FROM alpine@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11

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
