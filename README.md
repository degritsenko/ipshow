# ipshow

A small Go service that returns your public IP. The browser view shows country/city, CLI examples, and basic request counters.

Try it live at [gritsenko.me](https://gritsenko.me).

## Run with Docker Compose

Set your domain in `docker-compose.yaml` under `caddy.environment.DOMAIN`, without a scheme or path:

```yaml
environment:
  DOMAIN: example.com
```

Point the domain's DNS records to your server and make ports 80 and 443 reachable. Caddy reads `DOMAIN` from its environment and manages HTTPS certificates.

```bash
docker compose up -d --build
```

To update:

```bash
git pull
docker compose up -d --build
```

## Usage

```bash
curl example.com        # Plain-text IP
curl example.com/ip     # Plain-text IP, regardless of User-Agent
curl example.com/json   # IP and location as JSON
```

JSON fields: `ip`, `city`, `region`, `country` (country code), `loc` (latitude,longitude), `org` (ASN and organization), `postal`, and `timezone`. Unavailable fields are empty strings.

Browser requests over HTTP redirect to HTTPS; CLI requests are served over HTTP without a redirect. HTTPS works for both.

Containers: `ipshow` and `ipshow-caddy`. Keep the application port unpublished: forwarded IP headers are trusted only from the `caddy` service resolved through Docker DNS.

Go source and tests are in `app/`. Run tests with `go test ./...`.
