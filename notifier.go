package main

import (
	"time"

	sendgrid "github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
)

type notifier struct {
	apiKey string

	fromName, fromEmail string
	toEmails            []string
}

func (n notifier) notify(ts []Tender) error {
	if len(ts) == 0 {
		return nil
	}

	if len(n.toEmails) == 0 {
		return nil
	}

	hmsg := "<p>These new HRM tenders have appeared:</p>\n\n"

	const df = "Mon, 02 Jan 2006"
	for _, t := range ts {
		hmsg += "<h3><a href=\"" + t.URL + "\">" + t.Description + "</a></h3>\n"
		hmsg += "Issued " + t.IssuedDate.Format(df) + " and closing " + t.CloseDate.Format(df) + "\n\n"
	}

	from := mail.NewEmail(n.fromName, n.fromEmail)

	var tos []*mail.Email
	for _, te := range n.toEmails {
		em, err := mail.ParseEmail(te)
		if err != nil {
			return err
		}
		tos = append(tos, em)
	}

	emsg := mail.NewContent("text/html", hmsg)

	email := mail.NewV3Mail()
	email.SetFrom(from)
	email.Subject = "New HRM Tenders at " + time.Now().Format(time.RFC822)
	pers := mail.NewPersonalization()
	pers.AddTos(from)
	pers.AddBCCs(tos...)
	email.AddPersonalizations(pers)
	email.AddContent(emsg)

	client := sendgrid.NewSendClient(n.apiKey)
	_, err := client.Send(email)
	return err
}
