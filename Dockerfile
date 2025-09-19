FROM debian@sha256:833c135acfe9521d7a0035a296076f98c182c542a2b6b5a0fd7063d355d696be AS builder

ARG GO_VERSION=1.25.1

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

COPY go.mod go.sum ./
RUN /usr/local/go/bin/go mod download

COPY migrate.go .
RUN /usr/local/go/bin/go build -o wp-to-wpm migrate.go \
    && mv wp-to-wpm /usr/local/bin/wp-to-wpm

FROM debian@sha256:833c135acfe9521d7a0035a296076f98c182c542a2b6b5a0fd7063d355d696be

ENV DOCKER_USER=wpm
ENV ACTION_WORKDIR=/code

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

RUN useradd -m -s /bin/bash $DOCKER_USER \
    && mkdir -p $ACTION_WORKDIR \
    && chown -R $DOCKER_USER $ACTION_WORKDIR

COPY --from=builder /usr/local/bin/wpm /usr/local/bin/wpm
COPY --from=builder /usr/local/bin/wp-to-wpm /usr/local/bin/wp-to-wpm

COPY migrate.sh /usr/local/bin/migrate
RUN chmod +x /usr/local/bin/migrate

USER $DOCKER_USER

WORKDIR $ACTION_WORKDIR

CMD [ "/usr/local/bin/migrate" ]