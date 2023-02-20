package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/joeshaw/envdecode"
	_ "modernc.org/sqlite"
)

// currently requires a locally applied fix for https://github.com/golang/go/issues/44591 to
// work properly, such as the range over raw in
// https://github.com/golang/go/issues/44591#issuecomment-825100135.

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

	cl, err := NewClient("https://procurement-portal.novascotia.ca")
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

type Client struct {
	u     *url.URL
	c     *http.Client
	ready bool
	token string
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

	return &Client{u, c, false, ""}, nil
}

var baseHeaders = map[string]string{
	"Accept":       "application/json",
	"Content-Type": "application/json",
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

	type filter struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	body := struct {
		Filters []filter `json:"filters"`
	}{
		Filters: []filter{{"procurementEntity", "Halifax Regional Municipality"}},
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}

	q := make(url.Values)
	q.Set("page", strconv.Itoa(page))
	q.Set("numberOfRecords", "25")
	q.Set("sortType", "DATE_CREATED_DESC")

	u, err := f.u.Parse("/procurementui/tenders")
	if err != nil {
		return nil, "", err
	}

	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	for k, v := range baseHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("got status %d", resp.StatusCode)
	}

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	type tenderData struct {
		TenderID          string
		Title             string
		ProcurementEntity string
		PostDate          string
		ClosingDate       string
	}
	var respBody struct {
		TenderDataList []tenderData
	}

	if err := json.Unmarshal(b, &respBody); err != nil {
		return nil, "", err
	}

	var ts []Tender
	for _, td := range respBody.TenderDataList {
		pd, err := time.Parse("2006-01-02", td.PostDate)
		if err != nil {
			return nil, "", err
		}
		if len(td.ClosingDate) < len(time.DateOnly) {
			return nil, "", fmt.Errorf("bad closing date %q", td.ClosingDate)
		}
		cd, err := time.Parse(time.DateOnly, td.ClosingDate[:len(time.DateOnly)])
		if err != nil {
			return nil, "", err
		}

		ts = append(ts, Tender{
			ID:          td.TenderID,
			URL:         f.u.String() + "/tenders/" + td.TenderID,
			Description: td.Title,
			Agency:      td.ProcurementEntity,
			IssuedDate:  pd,
			CloseDate:   cd,
		})
	}

	token = ""
	if len(ts) > 0 {
		page++
		token = fmt.Sprint(page)
	}

	return ts, token, nil
}

func (f *Client) init(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "POST", f.u.String()+"/procurementui/authenticate", strings.NewReader(`{"rpid":"GUEST"}`))
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

	var body struct {
		JWTToken string
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return fmt.Errorf("unmarshaling body: %w", err)
	}

	if body.JWTToken == "" {
		return fmt.Errorf("body does not contain jwttoken")
	}

	f.token = body.JWTToken

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
