package main

import (
	"bytes"
	"html/template"
	"net/http"
)

var homeTemplate = template.Must(template.New("home").Parse(htmlTemplate))

type homePage struct {
	IP      string
	Domain  string
	Country string
	City    string
	Stats   basicStats
}

func renderHome(w http.ResponseWriter, page homePage) error {
	var body bytes.Buffer
	if err := homeTemplate.Execute(&body, page); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := body.WriteTo(w)
	return err
}

const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>IP Check</title>
    <style>
        body {
            margin: 0;
            background: #ffffff;
            color: #111111;
            font-family: "SF Mono", "Menlo", "Consolas", "Liberation Mono", monospace;
            padding: 28px 20px;
            line-height: 1.45;
        }

        .container {
            width: min(760px, 100%);
            margin: 0 auto;
        }

        .label {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.06em;
            color: #444444;
            margin-bottom: 8px;
        }

        .ip {
            margin: 0;
            font-size: clamp(2rem, 5.2vw, 3.1rem);
            font-weight: 700;
            letter-spacing: 0.01em;
            word-break: break-word;
        }

        .location {
            margin-top: 6px;
            font-size: 1rem;
            color: #222222;
        }

        .sep {
            margin: 18px 0 14px;
            color: #666666;
            font-size: 0.95rem;
        }

        .line {
            margin: 2px 0;
            font-size: 0.8rem;
            color: #666666;
        }

        .footer {
            margin-top: 18px;
            padding-top: 10px;
            border-top: 1px solid #d4d4d4;
            font-size: 1rem;
        }

        .usage {
            margin: 18px 0 0;
            font-size: 0.95rem;
            font-weight: 600;
            color: #111111;
        }

        .usage a,
        .footer a {
            color: #111111;
            text-decoration: underline;
            text-underline-offset: 3px;
        }

        .usage a:hover,
        .footer a:hover {
            color: #555555;
        }

        @media (max-width: 640px) {
            body { padding: 18px 14px; }
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="label">Your IP Address</div>
        <h1 class="ip">{{.IP}}</h1>
        <div class="location">{{.Country}}, {{.City}}</div>
        <p class="usage">Use curl, wget or HTTPie: plain text at <a href="/">{{.Domain}}</a> or <a href="/ip">/ip</a>, JSON at <a href="/json">/json</a>.</p>
        <div class="sep">---</div>
        <div class="line">Total requests: {{.Stats.TotalRequests}}</div>
        <div class="line">Requests today (UTC): {{.Stats.TodayRequests}}</div>
        <div class="line">CLI requests: {{.Stats.CLIRequests}}</div>
        <div class="line">JSON requests: {{.Stats.JSONRequests}}</div>
        <div class="line">Browser requests: {{.Stats.BrowserRequests}}</div>
        <div class="line">Stats since: {{.Stats.StatsSince}}</div>
        <div class="footer">
            <a href="https://github.com/degritsenko/ipshow" target="_blank" rel="noopener noreferrer">GitHub repo</a>
        </div>
    </div>
</body>
</html>`
