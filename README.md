# ipshow

Simple service that returns your public IP address, keeps visit stats, and shows country/city info in web view.

## Endpoints
- `/` - web page with IP, country, and city
- `/stats` - plain text stats by IP

CLI tools (`curl`, `wget`, `httpie`) get raw IP only.

## Run with Docker Compose
```bash
docker compose up -d --build
```

Before start, set your own domain in `/Users/gritsenko/Documents/New project/ipshow-push/Caddyfile`.

Containers:
- `ipshow`
- `ipshow-caddy`
