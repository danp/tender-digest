package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
	_ "modernc.org/sqlite"
)

// currently requires a locally applied fix for https://github.com/golang/go/issues/44591 to
// work properly, such as the range over raw in
// https://github.com/golang/go/issues/44591#issuecomment-825100135.

func main() {
	fs := flag.NewFlagSet("tender-digest", flag.ExitOnError)
	var dbFile string
	var skipNotify bool
	fs.StringVar(&dbFile, "db-file", "store.db", "sqlite database filename")
	fs.BoolVar(&skipNotify, "skip-notify", false, "skip notification, such as for initializing store")
	fs.Parse(os.Args[1:])

	var (
		sendgridAPIKey = os.Getenv("SENDGRID_API_KEY")
		fromName       = os.Getenv("FROM_NAME")
		fromEmail      = os.Getenv("FROM_EMAIL")
		toEmails       = os.Getenv("TO_EMAILS")
	)

	ctx := context.Background()

	cl, err := NewClient("https://halifax.bidsandtenders.ca/Module/Tenders/en")
	if err != nil {
		log.Fatal(err)
	}
	defer cl.Close()

	db, err := sql.Open("sqlite", "file:"+dbFile+"?_time_format=sqlite")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec("create table if not exists tenders (id text primary key, url text, description text, agency text, issued datetime, close datetime, first_observed datetime)"); err != nil {
		log.Fatal(err)
	}

	st := store{db}

	nt, err := findNew(ctx, cl, st)
	if err != nil {
		log.Fatal(err)
	}

	if sendgridAPIKey == "" || skipNotify {
		for _, t := range nt {
			fmt.Println(t.ID, t.Description)
		}
		return
	}

	not := notifier{
		apiKey:    sendgridAPIKey,
		fromName:  fromName,
		fromEmail: fromEmail,
		toEmails:  strings.Split(toEmails, ";"),
	}

	if err := not.notify(nt); err != nil {
		log.Fatal(err)
	}
}

type Client struct {
	u           *url.URL
	pw          *playwright.Playwright
	b           playwright.Browser
	p           playwright.Page
	ready       bool
	responsesMu sync.Mutex
	responses   []RawTenders
}

func NewClient(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{u: u}, nil
}

type Tender struct {
	ID          string
	URL         string
	Description string
	Agency      string
	IssuedDate  time.Time
	CloseDate   time.Time
}

var squeezeRe = regexp.MustCompile(`\s+`)
var unprintableRe = regexp.MustCompile(`[[:^print:]]`)

func (c *Client) List(ctx context.Context, token string) (_ []Tender, nextToken string, _ error) {
	if err := c.init(ctx); err != nil {
		return nil, "", err
	}

	if token != "" {
		next := c.p.GetByLabel("next page")
		if err := next.Click(); err != nil {
			return nil, "", fmt.Errorf("clicking next: %w", err)
		}
	}

	err := c.p.Locator("#myRepeater > div.repeater-viewport > div.repeater-canvas.borderless-grid > div > div > table > tbody").WaitFor()
	if err != nil {
		return nil, "", fmt.Errorf("waiting for table: %w", err)
	}

	c.responsesMu.Lock()
	defer c.responsesMu.Unlock()

	if len(c.responses) == 0 {
		return nil, "", errors.New("no responses")
	}

	r := c.responses[0]
	c.responses = c.responses[1:]

	var tenders []Tender
	for _, d := range r.Data {
		var t Tender

		id, rest, ok := strings.Cut(d.Title, " ")
		if !ok {
			return nil, "", fmt.Errorf("cutting title %q", d.Title)
		}

		rest = strings.TrimSpace(rest)
		rest = strings.TrimPrefix(rest, "-")
		rest = unprintableRe.ReplaceAllString(rest, "")
		rest = strings.TrimSpace(rest)
		rest = squeezeRe.ReplaceAllString(rest, " ")

		t.ID = id
		t.URL = c.u.ResolveReference(&url.URL{Path: "/Module/Tenders/en/Tender/Detail/" + d.ID}).String()
		t.Description = rest
		t.Agency = "Halifax Regional Municipality"

		t.IssuedDate, err = time.Parse("Mon Jan 2, 2006 3:04:05 PM", d.DateAvailableDisplay)
		if err != nil {
			return nil, "", fmt.Errorf("parsing issued date: %w", err)
		}

		t.CloseDate, err = time.Parse("Mon Jan 2, 2006 3:04:05 PM", d.DateClosingDisplay)
		if err != nil {
			return nil, "", fmt.Errorf("parsing close date: %w", err)
		}

		now := time.Now()
		if t.IssuedDate.Year() == 9999 {
			t.IssuedDate = now
		}
		if t.CloseDate.Year() == 9999 {
			t.CloseDate = now
		}

		tenders = append(tenders, t)
	}

	time.Sleep(5 * time.Second)

	next := c.p.GetByLabel("next page")
	if ok, err := next.IsEnabled(playwright.LocatorIsEnabledOptions{Timeout: ptr(10000.0)}); err != nil {
		return nil, "", fmt.Errorf("checking next enabled: %w", err)
	} else if ok {
		nextToken = "next"
	}

	return tenders, nextToken, nil
}

func (c *Client) Close() error {
	if c.b != nil {
		if err := c.b.Close(); err != nil {
			return err
		}
		c.b = nil
	}
	if c.pw != nil {
		if err := c.pw.Stop(); err != nil {
			return err
		}
		c.pw = nil
	}
	return nil
}

func (c *Client) init(ctx context.Context) error {
	if c.ready {
		return nil
	}

	err := playwright.Install(&playwright.RunOptions{Verbose: false, Browsers: []string{"chromium"}})
	if err != nil {
		return fmt.Errorf("installing playwright: %w", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("running playwright: %w", err)
	}
	// playwright.BrowserTypeLaunchOptions{Headless: ptr(false)}
	browser, err := pw.Chromium.Launch()
	if err != nil {
		return fmt.Errorf("launching browser: %w", err)
	}
	bctx, err := browser.NewContext()
	if err != nil {
		return fmt.Errorf("creating context: %w", err)
	}
	page, err := bctx.NewPage()
	if err != nil {
		return fmt.Errorf("creating page: %w", err)
	}

	page.On("response", func(r playwright.Response) {
		if !strings.Contains(r.URL(), "/Module/Tenders/en/Tender/Search/") {
			return
		}

		go func() {
			b, err := r.Body()
			if err != nil {
				return
			}
			var rt RawTenders
			if err := json.Unmarshal(b, &rt); err != nil {
				return
			}
			c.responsesMu.Lock()
			defer c.responsesMu.Unlock()
			c.responses = append(c.responses, rt)
		}()
	})

	if _, err = page.Goto(c.u.String()); err != nil {
		return fmt.Errorf("going to page: %w", err)
	}

	// page.get_by_role("button", name="Open Toggle Filters").click()
	if err := page.GetByRole(*playwright.AriaRoleButton, playwright.PageGetByRoleOptions{Name: "Open Toggle Filters"}).Click(); err != nil {
		return fmt.Errorf("clicking open toggle filters: %w", err)
	}
	// page.get_by_label("all", exact=True).click()
	if err := page.GetByLabel("all", playwright.PageGetByLabelOptions{Exact: ptr(true)}).Click(); err != nil {
		return fmt.Errorf("clicking all: %w", err)
	}

	c.pw = pw
	c.b = browser
	c.p = page
	c.ready = true
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

func (s store) maxObserved() (time.Time, error) {
	var ts sql.NullString
	if err := s.db.QueryRow("select max(first_observed) from tenders").Scan(&ts); err != nil {
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
	max, err := st.maxObserved()
	if err != nil {
		return nil, err
	}
	cutoff := max
	if cutoff.IsZero() {
		cutoff = time.Now()
	}
	cutoff = cutoff.AddDate(0, -4, 0)

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

			if t.CloseDate.Before(cutoff) {
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

func ptr[T any](v T) *T {
	return &v
}

type RawTenders struct {
	Success bool `json:"success"`
	Data    []struct {
		ID                                         string `json:"Id"`
		Title                                      string `json:"Title"`
		Scope                                      string `json:"Scope"`
		Status                                     string `json:"Status"`
		Description                                string `json:"Description"`
		DateAvailable                              string `json:"DateAvailable"`
		DateAvailableDisplay                       string `json:"DateAvailableDisplay"` // Fri Nov 8, 2024 12:00:00 AM
		DatePlannedIssue                           any    `json:"DatePlannedIssue"`
		DatePlannedIssueDisplay                    string `json:"DatePlannedIssueDisplay"`
		DateClosing                                string `json:"DateClosing"`
		DateClosingDisplay                         string `json:"DateClosingDisplay"` // Mon Nov 25, 2024 2:00:59 PM
		DaysLeft                                   int    `json:"DaysLeft"`
		DaysLeftPublish                            int    `json:"DaysLeftPublish"`
		Submitted                                  int    `json:"Submitted"`
		PlanTakers                                 int    `json:"PlanTakers"`
		Advertisements                             int    `json:"Advertisements"`
		Documents                                  int    `json:"Documents"`
		Addendums                                  int    `json:"Addendums"`
		ShowSubmitted                              bool   `json:"ShowSubmitted"`
		ShowPlanTakers                             bool   `json:"ShowPlanTakers"`
		VendorIsRegistered                         bool   `json:"VendorIsRegistered"`
		VendorHasBidInProgress                     bool   `json:"VendorHasBidInProgress"`
		VendorHasMultipleActiveSubmissions         bool   `json:"VendorHasMultipleActiveSubmissions"`
		FirstSubmissionID                          string `json:"FirstSubmissionId"`
		ShowSubmitOnline                           bool   `json:"ShowSubmitOnline"`
		ShowRegisterAsPlanTaker                    bool   `json:"ShowRegisterAsPlanTaker"`
		AllowBidQuestionSubmission                 bool   `json:"AllowBidQuestionSubmission"`
		OnlyRegisteredPlantakersCanSubmitQuestions bool   `json:"OnlyRegisteredPlantakersCanSubmitQuestions"`
		IncludeSeconds                             bool   `json:"IncludeSeconds"`
		TimeZoneLabel                              string `json:"TimeZoneLabel"`
		IsEmployee                                 bool   `json:"IsEmployee"`
	} `json:"data"`
	Total int `json:"total"`
}
