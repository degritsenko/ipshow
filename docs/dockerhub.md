# ipshow

A small public IP service written in Go. Browsers get a page with country/city, CLI examples, and basic request counters. CLI clients get a plain-text IP or JSON.

- Source: [github.com/degritsenko/ipshow](https://github.com/degritsenko/ipshow)
- Live demo: [gritsenko.me](https://gritsenko.me)

## Run with Docker Compose

Create these two files in the same directory. Set `DOMAIN` to your own domain, without a scheme or path. Point its DNS records to your server and allow inbound TCP ports 80 and 443.

### docker-compose.yaml

```yaml
services:
  ipshow:
    image: gritsenko/ipshow:latest
    platform: linux/amd64
    container_name: ipshow
    volumes:
      - ./data:/root/data
    restart: unless-stopped

  caddy:
    image: caddy:latest
    container_name: ipshow-caddy
    environment:
      DOMAIN: example.com
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
    depends_on:
      - ipshow
    restart: unless-stopped

volumes:
  caddy_data:
```

### Caddyfile

```caddyfile
http://{$DOMAIN} {
    @browser header_regexp User-Agent (?i)(mozilla|chrome|safari|firefox|edg)
    handle @browser {
        redir https://{host}{uri} 308
    }

    handle {
        reverse_proxy ipshow:8080 {
            header_up X-Forwarded-For {remote_host}
            header_up -X-Real-IP
        }
    }
}

https://{$DOMAIN} {
    reverse_proxy ipshow:8080 {
        header_up X-Forwarded-For {remote_host}
        header_up -X-Real-IP
    }
}
```

### Start

```bash
docker compose pull
docker compose up -d
```

Open `https://example.com`, replacing the domain with your own. Caddy obtains and renews HTTPS certificates automatically. HTTP requests from browsers redirect to HTTPS; CLI requests work over HTTP without a redirect.

## Usage

```bash
curl example.com        # Plain-text IP
curl example.com/ip     # Plain-text IP, regardless of User-Agent
curl example.com/json   # IP and location as JSON
```

JSON fields: `ip`, `city`, `region`, `country` (country code), `loc` (latitude,longitude), `org` (ASN and organization), `postal`, and `timezone`. Unavailable fields are empty strings.

## Data and networking

The database is stored at `/root/data/stats.db`, mapped to `./data/stats.db` on the host. Request counters and the geolocation cache survive container replacement. Daily counters use UTC. Preserve the `data` directory when updating.

Keep the service names `ipshow` and `caddy` as shown above. The application trusts forwarded IP headers only from the `caddy` service resolved through Docker DNS. Do not publish the application's port 8080 directly when using Caddy.
