package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/joeshaw/envdecode"
	_ "modernc.org/sqlite"
)

var config struct {
	SendgridAPIKey string   `env:"SENDGRID_API_KEY,required"`
	FromName       string   `env:"FROM_NAME,required"`
	FromEmail      string   `env:"FROM_EMAIL,required"`
	ToEmails       []string `env:"TO_EMAILS,required"`
}

func main() {
	fs := flag.NewFlagSet("tender-digest", flag.ExitOnError)
	var dbFile string
	var skipNotify bool
	fs.StringVar(&dbFile, "db-file", "store.db", "sqlite database filename")
	fs.BoolVar(&skipNotify, "skip-notify", false, "skip notification, such as for initializing store")
	fs.Parse(os.Args[1:])
	envdecode.MustStrictDecode(&config)

	ctx := context.Background()

	cl, err := NewClient("https://procurement.novascotia.ca/ns-tenders.aspx")
	if err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+dbFile+"?_time_format=sqlite")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec("create table if not exists tenders (id text primary key, url text, description text, agency text, issued datetime, close datetime, first_observed datetime)"); err != nil {
		log.Fatal(err)
	}

	st := store{db}

	not := &notifier{
		apiKey:    config.SendgridAPIKey,
		fromName:  config.FromName,
		fromEmail: config.FromEmail,
		toEmails:  config.ToEmails,
	}

	nt, err := findNew(ctx, cl, st)
	if err != nil {
		log.Fatal(err)
	}

	if skipNotify {
		for _, t := range nt {
			fmt.Println(t.ID, t.Description)
		}
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

type Client struct {
	u     *url.URL
	c     *http.Client
	ready bool
	vals  url.Values
}

func NewClient(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	c := &http.Client{Jar: jar}

	return &Client{u, c, false, nil}, nil
}

var baseHeaders = map[string]string{
	"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8",
	"Accept-Language":           "en-US,en;q=0.9",
	"Cache-Control":             "max-age=0",
	"Upgrade-Insecure-Requests": "1",
	"User-Agent":                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/68.0.3440.106 Safari/537.36",
}

type Tender struct {
	ID          string
	URL         string
	Description string
	Agency      string
	IssuedDate  time.Time
	CloseDate   time.Time
}

func (f *Client) List(ctx context.Context, token string) (_ []Tender, nextToken string, _ error) {
	if !f.ready {
		if token != "" {
			return nil, "", fmt.Errorf("List must first be called with empty token")
		}
		if err := f.init(ctx); err != nil {
			return nil, "", err
		}
		f.ready = true
	}

	page := 1
	if token != "" {
		p, err := strconv.Atoi(token)
		if err != nil {
			return nil, "", err
		}
		page = p
	}

	vals := f.vals

	const pageSize = 100

	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddDateRange", "0")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$tbSearchTenderID", "")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$tbDescription", "")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddCategoryList", "0")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddDeptAgency", "Halifax Regional Municipality (HRM)")
	vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$ddPageSize", fmt.Sprint(pageSize))

	if page > 1 {
		vals.Set("__EVENTTARGET", "ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$GridView1")
		vals.Set("__EVENTARGUMENT", "Page$"+fmt.Sprint(page))
	} else {
		vals.Set("ctl00$ctl00$ctl00$ContentPlaceHolderDefault$childContent$u_NSTendersgrid_v2_2$btnFilter", "Filter")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", f.u.String(), strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, "", err
	}
	for k, v := range baseHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://procurement.novascotia.ca")
	req.Header.Set("Referer", f.u.String())

	resp, err := f.c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("got status %d", resp.StatusCode)
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}

	vals = make(url.Values)
	doc.Find("form#aspnetForm input[type=hidden]").Each(func(_ int, s *goquery.Selection) {
		n, _ := s.Attr("name")
		v, _ := s.Attr("value")
		vals.Set(n, v)
	})
	f.vals = vals

	var (
		c  = &coll{}
		ts []Tender
	)

	doc.Find("table#ctl00_ctl00_ctl00_ContentPlaceHolderDefault_childContent_u_NSTendersgrid_v2_2_GridView1 > tbody > tr").
		Not(".gridfooter").
		Each(c.each(func(_ int, s *goquery.Selection) error {
			if s.Find("th").Length() > 0 {
				return nil
			}
			if s.HasClass("ProcPagination") {
				return nil
			}

			var t Tender
			t.ID = s.Find("td:nth-child(2) a").Text()

			href, ok := s.Find("td:nth-child(2) a").Attr("href")
			if !ok {
				return fmt.Errorf("%s missing href", t.ID)
			}
			hurl, err := f.u.Parse(href)
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

			const dateFormat = "02 Jan 2006"

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
	if c.err != nil {
		return nil, "", c.err
	}

	token = ""
	if len(ts) >= pageSize {
		page++
		token = fmt.Sprint(page)
	}

	return ts, token, c.err
}

func (f *Client) init(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", f.u.String(), nil)
	if err != nil {
		return err
	}
	for k, v := range baseHeaders {
		req.Header.Set(k, v)
	}

	resp, err := f.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("got status %d", resp.StatusCode)
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(b))
	if err != nil {
		return err
	}

	vals := make(url.Values)
	doc.Find("form#aspnetForm input[type=hidden]").Each(func(_ int, s *goquery.Selection) {
		n, _ := s.Attr("name")
		v, _ := s.Attr("value")
		vals.Set(n, v)
	})
	f.vals = vals

	return nil
}

type store struct {
	db *sql.DB
}

const dateFormat = "2006-01-02"

func (s store) add(t Tender) (bool, error) {
	res, err := s.db.Exec("insert into tenders (id, url, description, agency, issued, close, first_observed) values (?, ?, ?, ?, ?, ?, ?) on conflict do nothing",
		t.ID, t.URL, t.Description, t.Agency, t.IssuedDate.Format(dateFormat), t.CloseDate.Format(dateFormat), time.Now(),
	)
	if err != nil {
		return false, fmt.Errorf("insert: %v", err)
	}

	ra, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("affected: %v", err)
	}

	return ra > 0, nil
}

func (s store) maxIssued() (time.Time, error) {
	var ts sql.NullString
	if err := s.db.QueryRow("select max(issued) from tenders").Scan(&ts); err != nil {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	t, err := time.Parse(dateFormat, ts.String)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

func findNew(ctx context.Context, cl *Client, st store) ([]Tender, error) {
	max, err := st.maxIssued()
	if err != nil {
		return nil, err
	}
	start := max
	if !start.IsZero() {
		start = start.AddDate(0, -1, 0)
	}

	var nt []Tender

	var token string
outer:
	for {
		ct, nextToken, err := cl.List(ctx, token)
		if err != nil {
			return nil, err
		}

		for _, t := range ct {
			isNew, err := st.add(t)
			if err != nil {
				return nil, err
			}
			if isNew {
				nt = append(nt, t)
			}

			if t.IssuedDate.Before(start) {
				break outer
			}
		}

		if nextToken == "" {
			break
		}
		token = nextToken
	}

	return nt, nil
}
