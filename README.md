# ipshow

Simple service that returns your public IP address, keeps visit stats, and shows country/city info in web view.

CLI tools (`curl`, `wget`, `httpie`) get raw IP only.

Use `curl http://your-domain/ip` to always get plain-text IP, regardless of User-Agent.

Use `curl http://your-domain/json` for JSON with IP, city, region, country code, coordinates, ASN/organization, postal code, and timezone. Unavailable fields are empty strings.

## Run with Docker Compose
```bash
docker compose up -d --build
```

Before start, set your own domain in Caddyfile.
HTTP behavior in Caddyfile: browsers are redirected from `80` to `443`, CLI requests are served on `80` without redirect.

Forwarded IP headers are accepted only from the `caddy` service resolved through Docker DNS (cached for 30 seconds, with an early recheck for an unfamiliar peer, limited to once per second). Keep the application port unpublished. When running directly without Caddy, the connection's peer address is used.

Containers:
- `ipshow`
- `ipshow-caddy`
