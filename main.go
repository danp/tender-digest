package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/joeshaw/envdecode"
)

const (
	dateFormat = "02 Jan 2006"
)

type tender struct {
	ID          string
	URL         string
	Description string
	Agency      string
	IssuedDate  time.Time
	CloseDate   time.Time
}

var (
	baseHeaders = map[string]string{
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8",
		"Accept-Language":           "en-US,en;q=0.9",
		"Cache-Control":             "max-age=0",
		"Upgrade-Insecure-Requests": "1",
		"User-Agent":                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/68.0.3440.106 Safari/537.36",
	}

	skipNotify = flag.Bool("skip-notify", false, "skip notification, such as for initializing store")
)

var config struct {
	SendgridAPIKey string   `env:"SENDGRID_API_KEY,required"`
	FromName       string   `env:"FROM_NAME,required"`
	FromEmail      string   `env:"FROM_EMAIL,required"`
	ToEmails       []string `env:"TO_EMAILS,required"`
}

func main() {
	flag.Parse()
	envdecode.MustStrictDecode(&config)

	fet := &fetcher{
		baseURL: "https://procurement.novascotia.ca/ns-tenders.aspx",
	}

	str := &store{
		dir: "store",
	}

	not := &notifier{
		apiKey:    config.SendgridAPIKey,
		fromName:  config.FromName,
		fromEmail: config.FromEmail,
		toEmails:  config.ToEmails,
	}

	nt, err := findNew(fet, str)
	if err != nil {
		log.Fatal(err)
	}

	if *skipNotify {
		return
	}

	if err := not.notify(nt); err != nil {
		log.Fatal(err)
	}
}

type coll struct {
	err error
}

func (c *coll) each(f func(_ int, s *goquery.Selection) error) func(_ int, s *goquery.Selection) {
	return func(x int, s *goquery.Selection) {
		if c.err != nil {
			return
		}
		c.err = f(x, s)
	}
}

type fetcher struct {
	baseURL string
}

func (f fetcher) fetch() ([]tender, error) {
	purl, err := url.Parse(f.baseURL)
	if err != nil {
		return nil, err
	}

	getReq, err := http.NewRequest("GET", purl.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range baseHeaders {
		getReq.Header.Set(k, v)
	}

	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		return nil, err
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != 200 {
		return nil, fmt.Errorf("got status %d", getResp.StatusCode)
	}

	b, err := ioutil.ReadAll(getResp.Body)
	if err != nil {
		return nil, err
	}

	getDoc, err := goquery.NewDocumentFromReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	vals := make(url.Values)
	getDoc.Find("form#aspnetForm input[type=hidden]").Each(func(_ int, s *goquery.Selection) {
		n, _ := s.Attr("name")
		v, _ := s.Attr("value")
		vals.Set(n, v)
	})

	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddDateRange", "0")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$tbSearchTenderID", "")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$tbDescription", "")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddCategoryList", "0")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddDeptAgency", "Halifax Regional Municipality (HRM)")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddPageSize", "100")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$btnFilter", "Filter")

	postReq, err := http.NewRequest("POST", purl.String(), strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, err
	}
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Origin", "https://procurement.novascotia.ca")
	postReq.Header.Set("Referer", purl.String())
	postReq.Header.Set("Cookie", getResp.Header.Get("Set-Cookie"))

	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		return nil, err
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != 200 {
		return nil, fmt.Errorf("got status %d", postResp.StatusCode)
	}

	b, err = ioutil.ReadAll(postResp.Body)
	if err != nil {
		return nil, err
	}

	postDoc, err := goquery.NewDocumentFromReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	var (
		c  = &coll{}
		ts []tender
	)

	postDoc.Find("table#ctl00_ctl00_ctl00_ContentPlaceHolderDefault_childContent_u_NSTendersgrid_v2_2_GridView1 > tbody > tr").
		Not(".gridfooter").
		Each(c.each(func(_ int, s *goquery.Selection) error {
			if s.Find("th").Length() > 0 {
				return nil
			}
			if s.HasClass("ProcPagination") {
				return nil
			}

			var t tender
			t.ID = s.Find("td:nth-child(2) a").Text()

			href, ok := s.Find("td:nth-child(2) a").Attr("href")
			if !ok {
				return fmt.Errorf("%s missing href", t.ID)
			}
			hurl, err := purl.Parse(href)
			if err != nil {
				return err
			}

			t.URL = hurl.String()
			t.Agency = strings.TrimSpace(s.Find("td:nth-child(2) span").Text())
			t.Description = strings.TrimSpace(s.Find("td:nth-child(3)").Text())

			spanStrings := s.Find("td:nth-child(4) span").Map(func(_ int, ss *goquery.Selection) string {
				return ss.Text()
			})
			if len(spanStrings) != 2 {
				return fmt.Errorf("expected 2 date spans for id %q, found %d", t.ID, len(spanStrings))
			}

			id, err := time.Parse(dateFormat, spanStrings[1])
			if err != nil {
				return err
			}
			t.IssuedDate = id

			cd, err := time.Parse(dateFormat, spanStrings[0])
			if err != nil {
				return err
			}
			t.CloseDate = cd

			ts = append(ts, t)
			return nil
		}))

	return ts, c.err
}

type store struct {
	dir string
}

func (s store) mark(id string) (bool, error) {
	p := filepath.Join(s.dir, id)

	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	f.Close()

	return true, nil
}

func findNew(fet *fetcher, st *store) ([]tender, error) {
	ct, err := fet.fetch()
	if err != nil {
		return nil, err
	}

	var nt []tender
	for _, t := range ct {
		isNew, err := st.mark(t.ID)
		if err != nil {
			return nil, err
		}
		if isNew {
			nt = append(nt, t)
		}
	}

	return nt, nil
}
