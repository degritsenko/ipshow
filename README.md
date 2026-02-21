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

Containers:
- `ipshow`
- `ipshow-caddy`
