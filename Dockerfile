FROM debian@sha256:01a723bf5bfb21b9dda0c9a33e0538106e4d02cce8f557e118dd61259553d598 AS builder

ARG GO_VERSION=1.25.3

WORKDIR /usr/src/app

RUN apt update \
	&& apt install -y --no-install-recommends wget ca-certificates

RUN wget https://github.com/trywpm/cli/releases/latest/download/wpm-linux-amd64 \
	&& wget https://github.com/trywpm/cli/releases/latest/download/wpm-linux-amd64.sha256 \
	&& sha256sum -c wpm-linux-amd64.sha256 \
	&& chmod +x wpm-linux-amd64 \
	&& mv wpm-linux-amd64 /usr/local/bin/wpm

RUN wget https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz \
	&& rm -rf /usr/local/go \
	&& tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz \
	&& rm go${GO_VERSION}.linux-amd64.tar.gz

ENV PATH="/usr/local/go/bin:${PATH}"

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-migrate cmd/migrate/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o wpm-update cmd/update/main.go

FROM debian@sha256:01a723bf5bfb21b9dda0c9a33e0538106e4d02cce8f557e118dd61259553d598

ENV DOCKER_USER=wpm
ENV ACTION_WORKDIR=/code

ARG USER_UID=1000
ARG USER_GID=1000

RUN set -ex \
	&& savedAptMark="$(apt-mark showmanual)" \
	&& apt-mark auto '.*' > /dev/null \
	&& apt update \
	&& apt install -y --no-install-recommends ca-certificates subversion \
	&& rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
	&& { [ -z "$savedAptMark" ] || apt-mark manual $savedAptMark > /dev/null; } \
	&& find /usr/local -type f -executable -exec ldd '{}' ';' \
	| awk '/=>/ { print $(NF-1) }' \
	| sort -u \
	| xargs -r dpkg-query --search \
	| cut -d: -f1 \
	| sort -u \
	| xargs -r apt-mark manual \
	&& apt-get purge -y --auto-remove -o APT::AutoRemove::RecommendsImportant=false

RUN groupadd -g $USER_GID $DOCKER_USER
RUN useradd -rm -d /code -s /bin/bash -g $USER_GID -u $USER_UID $DOCKER_USER

COPY --from=builder /usr/local/bin/wpm /usr/local/bin/wpm
COPY --from=builder /usr/src/app/wpm-update /usr/local/bin/update-wpm
COPY --from=builder /usr/src/app/wpm-migrate /usr/local/bin/migrate-wpm

COPY update.sh /usr/local/bin/update
COPY migrate.sh /usr/local/bin/migrate

RUN chmod +x /usr/local/bin/update
RUN chmod +x /usr/local/bin/migrate

USER $DOCKER_USER

WORKDIR $ACTION_WORKDIR

CMD ["/usr/local/bin/migrate"]
