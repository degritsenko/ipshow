package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type geoData struct {
	Country  string `json:"country"`
	Code     string `json:"code"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Loc      string `json:"loc"`
	ASN      string `json:"asn"`
	ASNName  string `json:"asn_name"`
	Postal   string `json:"postal"`
	Timezone string `json:"timezone"`
}

type geoService struct {
	client *http.Client
}

func newGeoService() *geoService {
	return &geoService{client: &http.Client{Timeout: 2 * time.Second}}
}

func coordinates(lat, lon *float64) string {
	if lat == nil || lon == nil {
		return ""
	}
	return strconv.FormatFloat(*lat, 'f', -1, 64) + "," + strconv.FormatFloat(*lon, 'f', -1, 64)
}

func (g *geoService) fetch(url string, result any) bool {
	resp, err := g.client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(result) == nil
}

func (g *geoService) lookupIPWhoIs(ip string) geoData {
	var p struct {
		Success   bool     `json:"success"`
		Country   string   `json:"country"`
		Code      string   `json:"country_code"`
		City      string   `json:"city"`
		Region    string   `json:"region"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		Postal    string   `json:"postal"`
		Timezone  struct {
			ID string `json:"id"`
		} `json:"timezone"`
		Connection struct {
			ASN int    `json:"asn"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if !g.fetch("https://ipwho.is/"+ip, &p) || !p.Success {
		return geoData{}
	}
	asn := ""
	if p.Connection.ASN > 0 {
		asn = fmt.Sprintf("AS%d", p.Connection.ASN)
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: coordinates(p.Latitude, p.Longitude), ASN: asn, ASNName: p.Connection.Org, Postal: p.Postal, Timezone: p.Timezone.ID}
}

func (g *geoService) lookupIPAPI(ip string) geoData {
	var p struct {
		Country   string   `json:"country_name"`
		Code      string   `json:"country"`
		City      string   `json:"city"`
		Region    string   `json:"region"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		ASN       string   `json:"asn"`
		Org       string   `json:"org"`
		Postal    string   `json:"postal"`
		Timezone  string   `json:"timezone"`
	}
	if !g.fetch("https://ipapi.co/"+ip+"/json/", &p) {
		return geoData{}
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: coordinates(p.Latitude, p.Longitude), ASN: p.ASN, ASNName: p.Org, Postal: p.Postal, Timezone: p.Timezone}
}

func (g *geoService) lookupGeoJS(ip string) geoData {
	var p struct {
		Country   string `json:"country"`
		Code      string `json:"country_code"`
		City      string `json:"city"`
		Region    string `json:"region"`
		Latitude  string `json:"latitude"`
		Longitude string `json:"longitude"`
		ASN       int    `json:"asn"`
		Org       string `json:"organization_name"`
		Timezone  string `json:"timezone"`
	}
	if !g.fetch("https://get.geojs.io/v1/ip/geo/"+ip+".json", &p) {
		return geoData{}
	}
	loc, asn := "", ""
	if p.Latitude != "" && p.Longitude != "" {
		loc = p.Latitude + "," + p.Longitude
	}
	if p.ASN > 0 {
		asn = fmt.Sprintf("AS%d", p.ASN)
	}
	return geoData{Country: p.Country, Code: p.Code, City: p.City, Region: p.Region, Loc: loc, ASN: asn, ASNName: p.Org, Timezone: p.Timezone}
}

func (g *geoService) Lookup(ip string) geoData {
	ip = strings.TrimSpace(ip)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return geoData{}
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return geoData{Country: "Local/Private", Code: "LAN", City: "Local/Private", ASN: "LAN", ASNName: "Local/Private"}
	}
	d := g.lookupIPWhoIs(ip)
	if strings.TrimSpace(d.Country) == "" {
		d = g.lookupIPAPI(ip)
	}
	if strings.TrimSpace(d.Country) == "" {
		d = g.lookupGeoJS(ip)
	}
	if strings.TrimSpace(d.Country) == "" {
		return geoData{}
	}
	return d
}
