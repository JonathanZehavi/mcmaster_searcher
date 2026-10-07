# Image for hosting the site on a cloud service (Render, Railway, Fly...).
# The same program as the desktop .exe, plus Chromium for the McMaster lookups.

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /mcmasterlist .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium ca-certificates fonts-liberation tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /mcmasterlist /usr/local/bin/mcmasterlist

# Data (the list, images, browser profile) lives on a persistent disk at /data.
ENV MCM_DATA_DIR=/data \
    MCM_NO_OPEN=1 \
    MCM_NO_SANDBOX=1 \
    MCM_BROWSER_PATH=/usr/bin/chromium \
    PORT=8642
VOLUME /data
EXPOSE 8642
CMD ["mcmasterlist"]
