package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// GoogleAPI calls the Sheets and Forms REST APIs with an access token. Base
// URLs are fields so tests can point them at a fake.
type GoogleAPI struct {
	SheetsBase string // https://sheets.googleapis.com
	FormsBase  string // https://forms.googleapis.com
	HTTP       *http.Client
}

func NewGoogleAPI() *GoogleAPI {
	return &GoogleAPI{SheetsBase: "https://sheets.googleapis.com", FormsBase: "https://forms.googleapis.com",
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (g *GoogleAPI) do(ctx context.Context, method, u, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		if e.Error.Message == "" {
			e.Error.Message = fmt.Sprintf("%.200s", b)
		}
		return fmt.Errorf("google API %s: HTTP %d: %s", req.URL.Path, res.StatusCode, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// CreateSpreadsheet makes a spreadsheet with the given tabs.
func (g *GoogleAPI) CreateSpreadsheet(ctx context.Context, token, title string, tabs []string) (id, link string, err error) {
	sheets := []map[string]any{}
	for _, t := range tabs {
		sheets = append(sheets, map[string]any{"properties": map[string]any{"title": t}})
	}
	var out struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		SpreadsheetURL string `json:"spreadsheetUrl"`
	}
	err = g.do(ctx, "POST", g.SheetsBase+"/v4/spreadsheets", token,
		map[string]any{"properties": map[string]any{"title": title}, "sheets": sheets}, &out)
	return out.SpreadsheetID, out.SpreadsheetURL, err
}

// ReplaceTabs clears each tab and writes its rows, in two batched calls.
func (g *GoogleAPI) ReplaceTabs(ctx context.Context, token, spreadsheetID string, tabs map[string][][]any, order []string) error {
	ranges := []string{}
	data := []map[string]any{}
	for _, t := range order {
		ranges = append(ranges, "'"+t+"'")
		data = append(data, map[string]any{"range": "'" + t + "'!A1", "values": tabs[t]})
	}
	base := g.SheetsBase + "/v4/spreadsheets/" + url.PathEscape(spreadsheetID) + "/values"
	if err := g.do(ctx, "POST", base+":batchClear", token, map[string]any{"ranges": ranges}, nil); err != nil {
		return err
	}
	return g.do(ctx, "POST", base+":batchUpdate", token, map[string]any{"valueInputOption": "RAW", "data": data}, nil)
}

// FormQuestion is one question of a Google Form.
type FormQuestion = domain.FormQuestion

type FormInfo struct {
	Title          string         `json:"title"`
	CollectsEmails bool           `json:"collects_emails"`
	Questions      []FormQuestion `json:"questions"`
}

// GetForm reads a form's title and questions.
func (g *GoogleAPI) GetForm(ctx context.Context, token, formID string) (*FormInfo, error) {
	var out struct {
		Info struct {
			Title         string `json:"title"`
			DocumentTitle string `json:"documentTitle"`
		} `json:"info"`
		Settings struct {
			EmailCollectionType string `json:"emailCollectionType"`
		} `json:"settings"`
		Items []struct {
			Title        string `json:"title"`
			QuestionItem *struct {
				Question struct {
					QuestionID string `json:"questionId"`
				} `json:"question"`
			} `json:"questionItem"`
		} `json:"items"`
	}
	if err := g.do(ctx, "GET", g.FormsBase+"/v1/forms/"+url.PathEscape(formID), token, nil, &out); err != nil {
		return nil, err
	}
	f := &FormInfo{Title: out.Info.Title, CollectsEmails: out.Settings.EmailCollectionType != "" &&
		out.Settings.EmailCollectionType != "DO_NOT_COLLECT"}
	if f.Title == "" {
		f.Title = out.Info.DocumentTitle
	}
	for _, it := range out.Items {
		if it.QuestionItem != nil {
			f.Questions = append(f.Questions, FormQuestion{ID: it.QuestionItem.Question.QuestionID, Title: it.Title})
		}
	}
	return f, nil
}

// FormResponse is one submitted response.
type FormResponse struct {
	ID              string
	RespondentEmail string
	Submitted       time.Time
	Answers         map[string][]string // question ID -> values
}

// ListResponses reads every response (all pages).
func (g *GoogleAPI) ListResponses(ctx context.Context, token, formID string) ([]FormResponse, error) {
	var all []FormResponse
	page := ""
	for i := 0; i < 100; i++ {
		u := g.FormsBase + "/v1/forms/" + url.PathEscape(formID) + "/responses?pageSize=500"
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		var out struct {
			Responses []struct {
				ResponseID        string    `json:"responseId"`
				RespondentEmail   string    `json:"respondentEmail"`
				LastSubmittedTime time.Time `json:"lastSubmittedTime"`
				Answers           map[string]struct {
					TextAnswers struct {
						Answers []struct {
							Value string `json:"value"`
						} `json:"answers"`
					} `json:"textAnswers"`
				} `json:"answers"`
			} `json:"responses"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := g.do(ctx, "GET", u, token, nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Responses {
			fr := FormResponse{ID: r.ResponseID, RespondentEmail: r.RespondentEmail, Submitted: r.LastSubmittedTime,
				Answers: map[string][]string{}}
			for q, a := range r.Answers {
				for _, v := range a.TextAnswers.Answers {
					fr.Answers[q] = append(fr.Answers[q], v.Value)
				}
			}
			all = append(all, fr)
		}
		if out.NextPageToken == "" {
			return all, nil
		}
		page = out.NextPageToken
	}
	return all, fmt.Errorf("form has too many response pages")
}
